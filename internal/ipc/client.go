package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
)

// NotifyHandler runs on the read loop's goroutine; long work should be dispatched.
type NotifyHandler func(ctx context.Context, params json.RawMessage)

// ErrConnClosed is returned by Call when the connection is torn down before
// the response arrives. Exposed as a sentinel so callers can errors.Is it.
var ErrConnClosed = errors.New("connection closed")

// Client speaks JSON-RPC 2.0 to an ipc.Server over a unix socket.
// Call multiplexes outbound requests by id and routes responses to pending channels.
// Notifications from the server are dispatched to handlers registered via OnNotify.
type Client struct {
	conn net.Conn
	fr   *FrameReader
	fw   *FrameWriter

	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan *Response

	handlersMu sync.RWMutex
	handlers   map[string]NotifyHandler

	closeOnce sync.Once
	closed    chan struct{}

	// ctx is cancelled by Close so notify handlers can observe disconnect
	// instead of running detached on context.Background().
	//nolint:containedctx // intentionally owned by the connection lifetime.
	ctx       context.Context
	ctxCancel context.CancelFunc
}

func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socketPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := &Client{
		conn:      conn,
		fr:        NewFrameReader(conn),
		fw:        NewFrameWriter(conn),
		pending:   map[uint64]chan *Response{},
		handlers:  map[string]NotifyHandler{},
		closed:    make(chan struct{}),
		ctx:       ctx,
		ctxCancel: cancel,
	}

	go c.readLoop()

	return c, nil
}

func (c *Client) Done() <-chan struct{} { return c.closed }

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
		close(c.closed)

		if c.ctxCancel != nil {
			c.ctxCancel()
		}

		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	})

	return nil
}

func (c *Client) OnNotify(method string, h NotifyHandler) {
	c.handlersMu.Lock()
	c.handlers[method] = h
	c.handlersMu.Unlock()
}

// Call sends a request and blocks until response, ctx cancel, or connection close.
// If result is non-nil, the response.Result JSON is decoded into it.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)

	raw, err := marshalParams(params)
	if err != nil {
		return err
	}

	body, err := encodeRequest(id, method, raw)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	ch := make(chan *Response, 1)

	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.fw.WriteFrame(body); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrConnClosed
	case resp, ok := <-ch:
		if !ok {
			return ErrConnClosed
		}

		if resp.Error != nil {
			return resp.Error
		}

		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("unmarshal result: %w", err)
			}
		}

		return nil
	}
}

func (c *Client) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}

	body, err := encodeNotification(method, raw)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	return c.fw.WriteFrame(body)
}

func (c *Client) readLoop() {
	defer func() { _ = c.Close() }()

	for {
		frame, err := c.fr.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Warn("ipc client read", "err", err)
			}

			return
		}

		c.dispatch(frame)
	}
}

func (c *Client) dispatch(frame []byte) {
	var probe struct {
		ID     *uint64         `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		slog.Warn("ipc client decode", "err", err)
		return
	}

	if probe.ID != nil && probe.Method == "" {
		// Hold mu through the send so a concurrent Close() cannot close ch
		// between the lookup and the send. ch is buffered cap 1 and each id
		// receives at most one response, so the send is non-blocking.
		c.mu.Lock()
		defer c.mu.Unlock()

		ch, ok := c.pending[*probe.ID]
		if !ok {
			slog.Warn("ipc client orphan response", "id", *probe.ID)
			return
		}

		ch <- &Response{Jsonrpc: JSONRPCVersion, ID: *probe.ID, Result: probe.Result, Error: probe.Error}

		return
	}

	if probe.Method != "" {
		c.handlersMu.RLock()
		h := c.handlers[probe.Method]
		c.handlersMu.RUnlock()

		if h == nil {
			return
		}

		ctx := c.ctx
		if ctx == nil {
			ctx = context.Background()
		}

		h(ctx, probe.Params)
	}
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}

	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}

	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}

	return b, nil
}
