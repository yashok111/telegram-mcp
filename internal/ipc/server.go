package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
)

type (
	MethodHandler       func(ctx context.Context, conn *Conn, params json.RawMessage) (result any, err *Error)
	ServerNotifyHandler func(ctx context.Context, conn *Conn, params json.RawMessage)
)

type Server struct {
	socketPath string

	handlersMu     sync.RWMutex
	methodHandlers map[string]MethodHandler
	notifyHandlers map[string]ServerNotifyHandler

	hooksMu      sync.Mutex
	onConnect    func(*Conn)
	onDisconnect func(*Conn)

	connsMu sync.Mutex
	conns   map[*Conn]struct{}
}

type Conn struct {
	server *Server
	netc   net.Conn
	fr     *FrameReader
	fw     *FrameWriter

	closeOnce sync.Once
	closed    chan struct{}

	Meta sync.Map // free-form per-shim state (shim_id, label, etc.)
}

func NewServer(socketPath string) *Server {
	return &Server{
		socketPath:     socketPath,
		methodHandlers: map[string]MethodHandler{},
		notifyHandlers: map[string]ServerNotifyHandler{},
		conns:          map[*Conn]struct{}{},
	}
}

func (s *Server) Handle(method string, h MethodHandler) {
	s.handlersMu.Lock()
	s.methodHandlers[method] = h
	s.handlersMu.Unlock()
}

func (s *Server) HandleNotify(method string, h ServerNotifyHandler) {
	s.handlersMu.Lock()
	s.notifyHandlers[method] = h
	s.handlersMu.Unlock()
}

func (s *Server) OnConnect(f func(*Conn)) {
	s.hooksMu.Lock()
	s.onConnect = f
	s.hooksMu.Unlock()
}

func (s *Server) OnDisconnect(f func(*Conn)) {
	s.hooksMu.Lock()
	s.onDisconnect = f
	s.hooksMu.Unlock()
}

func (s *Server) Listen(ctx context.Context) error {
	_ = os.Remove(s.socketPath)

	// Tighten umask around Listen so the kernel creates the socket inode with
	// 0600 atomically. Post-listen os.Chmod would leave a race window where
	// the socket is briefly world-accessible (modulo parent dir perms).
	oldMask := syscall.Umask(0o177)
	l, err := net.Listen("unix", s.socketPath)
	syscall.Umask(oldMask)

	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}

	// Belt-and-suspenders chmod in case the umask narrow window was widened by
	// a concurrent setter; harmless when the inode is already 0600.
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	go func() {
		<-ctx.Done()

		_ = l.Close()
	}()

	slog.Info("ipc server listening", "socket", s.socketPath)

	for {
		nc, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				slog.Info("ipc server stopping", "socket", s.socketPath)
				s.closeAllConns()

				return nil
			}

			return fmt.Errorf("accept: %w", err)
		}

		c := &Conn{
			server: s,
			netc:   nc,
			fr:     NewFrameReader(nc),
			fw:     NewFrameWriter(nc),
			closed: make(chan struct{}),
		}

		s.connsMu.Lock()
		s.conns[c] = struct{}{}
		s.connsMu.Unlock()

		slog.Info("ipc accept", "remote", nc.RemoteAddr().String())

		s.hooksMu.Lock()
		onConnect := s.onConnect
		s.hooksMu.Unlock()

		if onConnect != nil {
			onConnect(c)
		}

		go s.handleConn(ctx, c)
	}
}

func (s *Server) closeAllConns() {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()

	for c := range s.conns {
		_ = c.Close()
	}
}

func (s *Server) handleConn(ctx context.Context, c *Conn) {
	defer func() {
		_ = c.Close()

		s.connsMu.Lock()
		delete(s.conns, c)
		s.connsMu.Unlock()

		s.hooksMu.Lock()
		onDisconnect := s.onDisconnect
		s.hooksMu.Unlock()

		if onDisconnect != nil {
			onDisconnect(c)
		}
	}()

	for {
		frame, err := c.fr.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Warn("ipc server read", "err", err)
			}

			return
		}

		s.dispatch(ctx, c, frame)
	}
}

func (s *Server) dispatch(ctx context.Context, c *Conn, frame []byte) {
	var probe struct {
		ID     *uint64         `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		slog.Warn("ipc server decode", "err", err)
		return
	}

	if probe.Method == "" {
		return
	}

	if probe.ID == nil {
		s.handlersMu.RLock()
		h := s.notifyHandlers[probe.Method]
		s.handlersMu.RUnlock()

		if h != nil {
			h(ctx, c, probe.Params)
		}

		return
	}

	s.handlersMu.RLock()
	h := s.methodHandlers[probe.Method]
	s.handlersMu.RUnlock()

	if h == nil {
		_ = c.writeResponse(*probe.ID, nil, &Error{Code: CodeMethodNotFound, Message: "method not found: " + probe.Method})
		return
	}

	result, rpcErr := h(ctx, c, probe.Params)
	_ = c.writeResponse(*probe.ID, result, rpcErr)
}

func (c *Conn) writeResponse(id uint64, result any, rpcErr *Error) error {
	var (
		raw json.RawMessage
		err error
	)

	if rpcErr == nil && result != nil {
		raw, err = json.Marshal(result)
		if err != nil {
			rpcErr = &Error{Code: CodeInternal, Message: err.Error()}
		}
	}

	body, err := encodeResponse(id, raw, rpcErr)
	if err != nil {
		return err
	}

	return c.fw.WriteFrame(body)
}

// encodeResponse builds the JSON-RPC 2.0 response envelope in a single pass:
// the already-marshaled result bytes are embedded verbatim alongside the
// envelope scaffolding, avoiding a second json.Marshal over the wrapper.
func encodeResponse(id uint64, result json.RawMessage, rpcErr *Error) ([]byte, error) {
	var buf bytes.Buffer

	buf.Grow(64 + len(result))
	buf.WriteString(`{"jsonrpc":"`)
	buf.WriteString(JSONRPCVersion)
	buf.WriteString(`","id":`)
	buf.WriteString(strconv.FormatUint(id, 10))

	switch {
	case rpcErr != nil:
		buf.WriteString(`,"error":`)

		errBytes, err := json.Marshal(rpcErr)
		if err != nil {
			return nil, fmt.Errorf("marshal error: %w", err)
		}

		buf.Write(errBytes)
	case len(result) > 0:
		buf.WriteString(`,"result":`)
		buf.Write(result)
	}

	buf.WriteByte('}')

	return buf.Bytes(), nil
}

func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}

	body, err := encodeNotification(method, raw)
	if err != nil {
		return err
	}

	return c.fw.WriteFrame(body)
}

// encodeNotification builds the JSON-RPC 2.0 notification envelope without
// running json.Marshal over the wrapper. Params bytes are embedded verbatim.
func encodeNotification(method string, params json.RawMessage) ([]byte, error) {
	methodJSON, err := json.Marshal(method)
	if err != nil {
		return nil, fmt.Errorf("marshal method: %w", err)
	}

	var buf bytes.Buffer

	buf.Grow(48 + len(methodJSON) + len(params))
	buf.WriteString(`{"jsonrpc":"`)
	buf.WriteString(JSONRPCVersion)
	buf.WriteString(`","method":`)
	buf.Write(methodJSON)

	if len(params) > 0 {
		buf.WriteString(`,"params":`)
		buf.Write(params)
	}

	buf.WriteByte('}')

	return buf.Bytes(), nil
}

// encodeRequest builds the JSON-RPC 2.0 request envelope, embedding the
// already-marshaled params bytes without a second marshal pass.
func encodeRequest(id uint64, method string, params json.RawMessage) ([]byte, error) {
	methodJSON, err := json.Marshal(method)
	if err != nil {
		return nil, fmt.Errorf("marshal method: %w", err)
	}

	var buf bytes.Buffer

	buf.Grow(64 + len(methodJSON) + len(params))
	buf.WriteString(`{"jsonrpc":"`)
	buf.WriteString(JSONRPCVersion)
	buf.WriteString(`","id":`)
	buf.WriteString(strconv.FormatUint(id, 10))
	buf.WriteString(`,"method":`)
	buf.Write(methodJSON)

	if len(params) > 0 {
		buf.WriteString(`,"params":`)
		buf.Write(params)
	}

	buf.WriteByte('}')

	return buf.Bytes(), nil
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.netc.Close()
		close(c.closed)
	})

	return nil
}

func (c *Conn) Done() <-chan struct{} { return c.closed }
