package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
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

	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}

	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	go func() {
		<-ctx.Done()

		_ = l.Close()
	}()

	for {
		nc, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
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
	resp := Response{Jsonrpc: JSONRPCVersion, ID: id}

	if rpcErr != nil {
		resp.Error = rpcErr
	} else if result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			resp.Error = &Error{Code: CodeInternal, Message: err.Error()}
		} else {
			resp.Result = raw
		}
	}

	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}

	return c.fw.WriteFrame(body)
}

func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}

	body, err := json.Marshal(Notification{Jsonrpc: JSONRPCVersion, Method: method, Params: raw})
	if err != nil {
		return err
	}

	return c.fw.WriteFrame(body)
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.netc.Close()
		close(c.closed)
	})

	return nil
}

func (c *Conn) Done() <-chan struct{} { return c.closed }
