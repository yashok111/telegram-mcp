package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// DefaultShimLogMaxBytes caps a single shim's log file before it rotates to
// <path>.1. Per-shim files are smaller than daemon.log because each holds
// only events tagged with that shim_id.
const DefaultShimLogMaxBytes = 2 * 1024 * 1024

// ShimLogs owns per-shim append-only log files under <stateDir>/shims/.
// File handles open on Router.Register (via Daemon.openShimLog) and close on
// disconnect; files remain on disk so admin tooling and the sweep can read or
// delete them after the shim is gone.
type ShimLogs struct {
	dir      string
	maxBytes int64

	mu    sync.Mutex
	files map[string]*shimLogFile
}

type shimLogFile struct {
	path string

	mu   sync.Mutex
	f    *os.File
	size int64
}

// NewShimLogs creates <stateDir>/shims with 0700 and returns a sink. A
// non-positive maxBytes disables per-file rotation; files grow without bound
// until the sweep removes them or the operator truncates manually.
func NewShimLogs(dir string, maxBytes int64) (*ShimLogs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir shims: %w", err)
	}

	return &ShimLogs{
		dir:      dir,
		maxBytes: maxBytes,
		files:    map[string]*shimLogFile{},
	}, nil
}

// Open creates (or appends to) <dir>/<shim_id>.log and remembers the handle
// for subsequent Write calls. Idempotent: a second Open for the same shim_id
// is a no-op. shimID="" is a no-op.
func (s *ShimLogs) Open(shimID string) error {
	if s == nil || shimID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.files[shimID]; exists {
		return nil
	}

	path := filepath.Join(s.dir, shimLogFilename(shimID))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open shim log %s: %w", path, err)
	}

	var size int64

	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}

	s.files[shimID] = &shimLogFile{path: path, f: f, size: size}

	return nil
}

// Close releases the file handle for shimID. The file stays on disk so the
// sweep can age it out and admin tools can still read it. Calling Close on an
// unknown shimID is a no-op.
func (s *ShimLogs) Close(shimID string) {
	if s == nil || shimID == "" {
		return
	}

	s.mu.Lock()
	file, ok := s.files[shimID]

	if ok {
		delete(s.files, shimID)
	}

	s.mu.Unlock()

	if !ok {
		return
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	if file.f != nil {
		_ = file.f.Close()
		file.f = nil
	}
}

// CloseAll releases every open per-shim handle. Called on daemon shutdown.
func (s *ShimLogs) CloseAll() {
	if s == nil {
		return
	}

	s.mu.Lock()
	files := s.files
	s.files = map[string]*shimLogFile{}
	s.mu.Unlock()

	for _, f := range files {
		f.mu.Lock()

		if f.f != nil {
			_ = f.f.Close()
			f.f = nil
		}

		f.mu.Unlock()
	}
}

// IsOpen reports whether a handle is currently held for shimID. Used by the
// sweep to skip files belonging to active shims regardless of mtime.
func (s *ShimLogs) IsOpen(shimID string) bool {
	if s == nil || shimID == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.files[shimID]

	return ok
}

// Write appends b to the per-shim file. Drops silently when shimID is empty,
// no file is open, or write fails — slog inside a handler would recurse so
// errors stay quiet; the daemon.log path is the canonical record.
func (s *ShimLogs) Write(shimID string, b []byte) {
	if s == nil || shimID == "" || len(b) == 0 {
		return
	}

	s.mu.Lock()
	file, ok := s.files[shimID]
	s.mu.Unlock()

	if !ok {
		return
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	if file.f == nil {
		return
	}

	n, err := file.f.Write(b)
	if err != nil {
		return
	}

	file.size += int64(n)

	if s.maxBytes > 0 && file.size >= s.maxBytes {
		_ = s.rotateLocked(file)
	}
}

// rotateLocked renames file.path → file.path+".1" (replacing prior) and
// reopens path as a fresh file. Caller holds file.mu.
//
// Any error path nils out file.f so subsequent Write calls become no-ops
// instead of thrashing: without that, the next Write would still see size
// over maxBytes and re-enter rotateLocked on every call.
func (s *ShimLogs) rotateLocked(file *shimLogFile) error {
	if file.f == nil {
		return nil
	}

	rotated := file.path + ".1"
	_ = os.Remove(rotated)

	if err := os.Rename(file.path, rotated); err != nil && !os.IsNotExist(err) {
		_ = file.f.Close()
		file.f = nil
		file.size = 0

		return fmt.Errorf("rename: %w", err)
	}

	_ = file.f.Close()

	newF, err := os.OpenFile(file.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		file.f = nil
		file.size = 0

		return fmt.Errorf("reopen: %w", err)
	}

	file.f = newF
	file.size = 0

	return nil
}

// shimLogFilename returns the on-disk filename for a shim_id's log. Kept as a
// helper so the sweep and Open agree on the same naming convention.
func shimLogFilename(shimID string) string {
	return shimID + ".log"
}

// ShimLogHandler is a slog.Handler that always delegates to inner (the
// process-wide stderr → daemon.log path) and, when the record carries a
// `shim_id` attr, additionally serializes the record as JSON and writes it
// to ShimLogs for that shim.
//
// shim_id may appear in r.Attrs (inline call site) or in attrs accumulated
// via WithAttrs (rare in daemon code today, but covered for safety).
type ShimLogHandler struct {
	inner slog.Handler
	sink  *ShimLogs

	attrs  []slog.Attr
	groups []string
}

// NewShimLogHandler wraps inner. sink may be nil — Handle will then behave
// exactly like inner.
func NewShimLogHandler(inner slog.Handler, sink *ShimLogs) *ShimLogHandler {
	return &ShimLogHandler{inner: inner, sink: sink}
}

func (h *ShimLogHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

func (h *ShimLogHandler) Handle(ctx context.Context, r slog.Record) error {
	innerErr := h.inner.Handle(ctx, r)

	if h.sink != nil {
		if shimID := h.extractShimID(r); shimID != "" {
			if line, err := serializeRecord(ctx, r, h.attrs, h.groups); err == nil {
				h.sink.Write(shimID, line)
			}
		}
	}

	return innerErr
}

func (h *ShimLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)

	return &ShimLogHandler{
		inner:  h.inner.WithAttrs(attrs),
		sink:   h.sink,
		attrs:  merged,
		groups: h.groups,
	}
}

func (h *ShimLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)

	return &ShimLogHandler{
		inner:  h.inner.WithGroup(name),
		sink:   h.sink,
		attrs:  h.attrs,
		groups: groups,
	}
}

// extractShimID scans accumulated attrs first (a pre-bound shim_id wins over
// the record), then the record's own attrs. Returns "" when no shim_id is set
// or the value is empty.
func (h *ShimLogHandler) extractShimID(r slog.Record) string {
	for _, a := range h.attrs {
		if a.Key == shimIDAttr {
			if s := attrString(a); s != "" {
				return s
			}
		}
	}

	var found string

	r.Attrs(func(a slog.Attr) bool {
		if a.Key == shimIDAttr {
			if s := attrString(a); s != "" {
				found = s
				return false
			}
		}

		return true
	})

	return found
}

const shimIDAttr = "shim_id"

func attrString(a slog.Attr) string {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindString {
		return v.String()
	}

	return ""
}

// serializeRecord encodes r as a single JSON line using a fresh JSONHandler
// pre-bound with the same accumulated attrs/groups so the per-shim line is
// identical in shape to the daemon.log line.
func serializeRecord(ctx context.Context, r slog.Record, attrs []slog.Attr, groups []string) ([]byte, error) {
	buf := &bytes.Buffer{}

	var h slog.Handler = slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})

	for _, g := range groups {
		h = h.WithGroup(g)
	}

	if len(attrs) > 0 {
		h = h.WithAttrs(attrs)
	}

	if err := h.Handle(ctx, r); err != nil {
		return nil, fmt.Errorf("serialize record: %w", err)
	}

	return buf.Bytes(), nil
}
