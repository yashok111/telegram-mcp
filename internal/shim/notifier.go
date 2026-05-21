package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/yakov/telegram-mcp/internal/ipc"
)

// MCPSink is the slice of mcp.Server the notifier writes into.
type MCPSink interface {
	DeliverInbound(content string, meta map[string]string)
	ResolvePermission(requestID, behavior string)
}

// StatsSink is an optional diagnostic interface — when MCPSink also implements
// it, the notifier logs MCP session state to shim-debug.log so we can verify
// notifications arrive only after the stdio handshake completed.
type StatsSink interface {
	SessionStats() (int32, int32)
}

// debugLogPath, if non-empty, receives one JSON line per inbound the shim
// pulls off the IPC wire. Used to verify daemon→shim delivery independently
// of MCP session lifecycle. Empty by default — set via AttachNotifierDebug.
var debugLogPath string

// AttachNotifierDebug wires AttachNotifier with a per-shim diagnostic log
// file at path. Empty path disables. Path is process-global; the last call
// wins (we expect one shim per process).
func AttachNotifierDebug(path string) { debugLogPath = path }

// notifierQueueCap bounds the off-read-loop worker channel. Telegram inbound
// is naturally rate-limited, so a depth of 64 absorbs short bursts without
// risking unbounded memory if the MCP sink stalls.
const notifierQueueCap = 64

// notifierWorker is a single-consumer serial dispatcher. Sink calls run here
// — not on the IPC read loop — so a slow DeliverInbound or UpdateLabel (e.g.
// mcp-go's per-session channel pushing back, a stuck FS) cannot stall
// daemon↔shim traffic. Ordering within the worker is preserved.
//
// The consumer goroutine starts lazily on the first submit so tests that
// only exercise Wire-without-Run (no notifications fire) do not leave a
// dangling goroutine behind. One worker per Shim lifetime.
//
// The IPC read loop calls submit from its own goroutine, possibly racing
// with Run's defer chain calling Stop. submit must not send on a closed
// channel; we coordinate via the stop channel (closed exactly once) and a
// select with default, never closing the work queue.
type notifierWorker struct {
	startOnce sync.Once
	stopOnce  sync.Once
	queue     chan func()
	stop      chan struct{}
	done      chan struct{}
}

func newNotifierWorker() *notifierWorker {
	return &notifierWorker{
		queue: make(chan func(), notifierQueueCap),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

func (w *notifierWorker) ensureStarted() {
	w.startOnce.Do(func() {
		go func() {
			defer close(w.done)

			for {
				select {
				case fn := <-w.queue:
					fn()
				case <-w.stop:
					// Drain anything already enqueued so a final
					// permission_resolved isn't lost on shutdown.
					for {
						select {
						case fn := <-w.queue:
							fn()
						default:
							return
						}
					}
				}
			}
		}()
	})
}

func (w *notifierWorker) submit(kind string, fn func()) {
	if w == nil {
		fn()
		return
	}

	w.ensureStarted()

	// Check stop first so a Stop in flight short-circuits before we touch
	// the queue. The select below also has a stop case, but checking up
	// front avoids racing with the consumer's drain loop.
	select {
	case <-w.stop:
		slog.Warn("notifier stopped, dropping", "kind", kind)

		return
	default:
	}

	select {
	case w.queue <- fn:
	case <-w.stop:
		slog.Warn("notifier stopped mid-send, dropping", "kind", kind)
	default:
		// Queue full — log and drop rather than block the read loop.
		// Telegram will re-deliver the inbound if the user resends; permission
		// timeouts will eventually retry. Better than a deadlocked IPC.
		slog.Warn("notifier queue saturated, dropping", "kind", kind, "cap", notifierQueueCap)
	}
}

// Stop signals the worker to drain and exit. Safe to call multiple times
// concurrently from any goroutine; safe to call before any submit (lazy-start
// means the consumer goroutine may have never been spawned — Stop kicks it
// off briefly so done can be closed cleanly).
func (w *notifierWorker) Stop() {
	if w == nil {
		return
	}

	w.ensureStarted()
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}

func AttachNotifier(c IPCClient, sink MCPSink, w *notifierWorker) {
	c.OnNotify(ipc.NotifyInbound, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Content string            `json:"content"`
			Meta    map[string]string `json:"meta"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("inbound unmarshal", "err", err)
			return
		}

		slog.Info("shim received inbound", "chat_id", p.Meta["chat_id"], "user", p.Meta["user"], "content_len", len(p.Content))

		w.submit("inbound", func() {
			preFields := map[string]any{
				"chat_id":     p.Meta["chat_id"],
				"user":        p.Meta["user"],
				"content_len": len(p.Content),
				"pid":         os.Getpid(),
			}

			if stats, ok := sink.(StatsSink); ok {
				r, i := stats.SessionStats()
				preFields["mcp_sessions_registered"] = r
				preFields["mcp_sessions_inited"] = i
			}

			writeDebug("inbound", preFields)

			sink.DeliverInbound(p.Content, p.Meta)

			postFields := map[string]any{
				"chat_id": p.Meta["chat_id"],
				"pid":     os.Getpid(),
			}

			if stats, ok := sink.(StatsSink); ok {
				r, i := stats.SessionStats()
				postFields["mcp_sessions_registered"] = r
				postFields["mcp_sessions_inited"] = i
			}

			writeDebug("inbound_delivered_to_mcp", postFields)
		})
	})

	c.OnNotify(ipc.NotifyPermissionResolved, func(_ context.Context, params json.RawMessage) {
		var p struct {
			RequestID string `json:"request_id"`
			Behavior  string `json:"behavior"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("perm resolved unmarshal", "err", err)
			return
		}

		w.submit("permission_resolved", func() {
			writeDebug("permission_resolved", map[string]any{
				"request_id": p.RequestID,
				"behavior":   p.Behavior,
				"pid":        os.Getpid(),
			})

			sink.ResolvePermission(p.RequestID, p.Behavior)
		})
	})
}

// LabelUpdater receives runtime label changes pushed by the daemon. The shim
// implements this to rewrite its sessionfile so `telegram-mcp self` and the
// statusline pick up the new label without a CC restart.
type LabelUpdater interface {
	UpdateLabel(label string)
}

// AttachLabelHandler registers the daemon→shim label-change notification handler.
// nil updater disables the handler (test-only path). UpdateLabel runs on the
// worker (off the IPC read loop) because it writes the sessionfile to disk.
func AttachLabelHandler(c IPCClient, updater LabelUpdater, w *notifierWorker) {
	if updater == nil {
		return
	}

	c.OnNotify(ipc.NotifyLabelChanged, func(_ context.Context, params json.RawMessage) {
		var p struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("label notify unmarshal", "err", err)
			return
		}

		slog.Info("shim received label change", "label", p.Label)
		w.submit("label_changed", func() { updater.UpdateLabel(p.Label) })
	})
}

func writeDebug(event string, fields map[string]any) {
	if debugLogPath == "" {
		return
	}

	fields["event"] = event
	fields["ts"] = time.Now().Format(time.RFC3339Nano)

	line, err := json.Marshal(fields)
	if err != nil {
		return
	}

	f, err := os.OpenFile(debugLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // debugLogPath is set internally from StateDir, not user input.
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	_, _ = fmt.Fprintln(f, string(line))
}
