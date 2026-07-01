package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
)

const (
	// askDefaultTimeout bounds a blocking ask when the operator never taps.
	// Overridable via SetAskConfig (TELEGRAM_ASK_TIMEOUT); it NEVER disables —
	// an unbounded ask would hold an mcp-go tool-call worker forever.
	askDefaultTimeout = 5 * time.Minute

	// askMinOptions / askDefaultMaxOptions bound the choice count. Two is the
	// minimum for a "choice"; ten keeps the keyboard tappable and the render
	// within one message.
	askMinOptions        = 2
	askDefaultMaxOptions = 10

	// askDefaultMaxConcurrent caps in-flight asks per shim. Kept at 4 (< the
	// mcp-go stdio tool-call worker pool of 5) so blocked asks can never
	// exhaust the pool and stall reply/react/edit on the remaining workers.
	askDefaultMaxConcurrent = 4

	// askDefaultMaxQuestionLen bounds the question body. The prompt is a single
	// non-chunked Telegram message (4096-byte cap); leave generous headroom for
	// the daemon's "@sN: " source prefix + the "❓ " marker.
	askDefaultMaxQuestionLen = 3500

	// askDefaultMaxLabel bounds a single option's button text.
	askDefaultMaxLabel = 100

	// qidAlphabet excludes 'l' to dodge 1/l ambiguity — same alphabet as the
	// permission request_id, so bot.askCallbackRE ([a-km-z]{5}) accepts it.
	qidAlphabet = "abcdefghijkmnopqrstuvwxyz"
	qidLen      = 5

	// askRegisterAttempts bounds qid-collision regeneration. The space is
	// 25^5 ≈ 9.8M and concurrent asks are single-digit, so one retry is already
	// astronomically sufficient; five is pure belt-and-suspenders.
	askRegisterAttempts = 5

	// askBroadcastTimeout bounds the synchronous BroadcastAsk IPC round-trip so
	// a stalled daemon (e.g. a wedged Telegram send) can't hold the mcp-go
	// tool-call worker forever — the answer-wait timeout only covers the wait
	// AFTER the broadcast succeeds.
	askBroadcastTimeout = 30 * time.Second

	// askCancelTimeout bounds the best-effort daemon-side forget after a
	// timeout/ctx-cancel. It runs on a fresh context so a cancelled caller ctx
	// doesn't also abort the cleanup.
	askCancelTimeout = 5 * time.Second
)

// askAnswer is delivered on a pendingAsk channel. A non-nil err means the wait
// was cancelled (daemon disconnect) rather than answered; idx/user carry the
// operator's choice otherwise.
type askAnswer struct {
	idx  int
	user string
	err  error
}

// SetAskConfig overrides the ask tunables (called from cmd/server wiring after
// New). A non-positive timeout keeps the current default — the tool must never
// block unbounded. Non-positive counts likewise keep defaults.
func (s *Server) SetAskConfig(timeout time.Duration, maxOptions, maxConcurrent int) {
	s.askMu.Lock()
	defer s.askMu.Unlock()

	if timeout > 0 {
		s.askTimeout = timeout
	}

	if maxOptions > 0 {
		s.askMaxOpts = maxOptions
	}

	if maxConcurrent > 0 {
		s.askMaxConc = maxConcurrent
	}
}

func newQID() (string, error) {
	b := make([]byte, qidLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random for qid: %w", err)
	}

	out := make([]byte, qidLen)
	for i, v := range b {
		out[i] = qidAlphabet[int(v)%len(qidAlphabet)]
	}

	return string(out), nil
}

// registerAsk allocates a fresh qid and its cap-1 answer channel, enforcing the
// per-shim concurrency cap. Returns a typed error when the cap is hit so
// handleAsk can surface "too many pending questions" to the agent.
func (s *Server) registerAsk() (string, chan askAnswer, error) {
	s.askMu.Lock()
	defer s.askMu.Unlock()

	if len(s.pendingAsk) >= s.askMaxConc {
		return "", nil, fmt.Errorf("too many pending questions (max %d) — wait for an answer before asking again", s.askMaxConc)
	}

	for range askRegisterAttempts {
		qid, err := newQID()
		if err != nil {
			return "", nil, err
		}

		if _, exists := s.pendingAsk[qid]; exists {
			continue
		}

		ch := make(chan askAnswer, 1)
		s.pendingAsk[qid] = ch

		return qid, ch, nil
	}

	return "", nil, errors.New("could not allocate a unique question id")
}

func (s *Server) unregisterAsk(qid string) {
	s.askMu.Lock()
	delete(s.pendingAsk, qid)
	s.askMu.Unlock()
}

// drainPendingAsks removes and returns every in-flight ask channel under the
// lock (defer-unlocked so a panic can't strand askMu), so callers send the
// cancellation OUTSIDE the lock — never holding askMu across a channel op.
func (s *Server) drainPendingAsks() []chan askAnswer {
	s.askMu.Lock()
	defer s.askMu.Unlock()

	chans := make([]chan askAnswer, 0, len(s.pendingAsk))
	for qid, ch := range s.pendingAsk {
		chans = append(chans, ch)

		delete(s.pendingAsk, qid)
	}

	return chans
}

// ResolveAsk delivers an operator's tapped answer to the blocked ask. Called
// from the shim notifier worker (single consumer). The send is NON-BLOCKING on
// the cap-1 channel and the entry is deleted under lock — never close()d — so a
// second (raced) tap, or a tap that lands after the ask already timed out, is a
// silent no-op instead of a panic or a wedged worker. Implements shim.MCPSink.
func (s *Server) ResolveAsk(qid string, idx int, user string) {
	s.askMu.Lock()
	ch, ok := s.pendingAsk[qid]

	if ok {
		delete(s.pendingAsk, qid)
	}

	s.askMu.Unlock()

	if !ok {
		slog.Info("ask answer dropped: no pending question", "qid", qid, "idx", idx)
		return
	}

	select {
	case ch <- askAnswer{idx: idx, user: user}:
	default:
	}
}

// CancelAllAsks fails every outstanding ask with a typed error so the blocked
// tool calls return promptly instead of waiting out the timeout. Invoked from
// the shim's disconnect seam (AC6): a routed answer can no longer arrive once
// the daemon link drops.
func (s *Server) CancelAllAsks(reason string) {
	chans := s.drainPendingAsks()
	if len(chans) == 0 {
		return
	}

	slog.Warn("cancelling pending asks", "count", len(chans), "reason", reason)

	err := errors.New(reason)

	for _, ch := range chans {
		select {
		case ch <- askAnswer{err: err}:
		default:
		}
	}
}

func (s *Server) handleAsk(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
	chatID := req.GetString("chat_id", "")
	question := req.GetString("question", "")
	options := req.GetStringSlice("options", nil)

	slog.Info("tool ask invoked", "chat_id", chatID, "question_len", len(question), "options", len(options))

	st := s.store.Load()
	if !access.Allowed(st, chatID) {
		slog.Warn("tool ask gate denied", "chat_id", chatID)
		return mcptypes.NewToolResultError(fmt.Sprintf("chat %s is not allowlisted — add via /telegram:access", chatID)), nil
	}

	if errMsg := s.validateAsk(question, options); errMsg != "" {
		return mcptypes.NewToolResultError(errMsg), nil
	}

	b := s.Bot()
	if b == nil {
		return mcptypes.NewToolResultError("telegram not connected — cannot ask right now"), nil
	}

	for range askRegisterAttempts {
		qid, ch, err := s.registerAsk()
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}

		bctx, bcancel := context.WithTimeout(ctx, askBroadcastTimeout)
		berr := b.BroadcastAsk(bctx, qid, question, options, chatID)

		bcancel()

		if berr == nil {
			return s.waitAsk(ctx, b, qid, ch, options), nil
		}

		if errors.Is(berr, bot.ErrAskIDInUse) {
			// The daemon REJECTED this qid (never registered it), so no cancel is
			// needed — just drop local state and try a fresh one.
			s.unregisterAsk(qid)
			slog.Warn("ask qid collided at daemon, regenerating", "qid", qid)

			continue
		}

		// A non-collision failure (e.g. the 30s broadcast bound expired while the
		// daemon was wedged) may have left the daemon holding a registered
		// askOwners entry. Forget it daemon-side FIRST (cancel-then-unregister,
		// matching waitAsk's ordering) so the local pending entry outlives the
		// cancel attempt; a no-op if the daemon never registered it.
		slog.Error("ask broadcast failed", "qid", qid, "err", berr)
		s.cancelAskOnDaemon(b, qid)
		s.unregisterAsk(qid)

		return mcptypes.NewToolResultError("could not deliver the question: " + berr.Error()), nil
	}

	return mcptypes.NewToolResultError("could not deliver the question after repeated id collisions"), nil
}

// waitAsk blocks until the operator answers, the caller ctx is cancelled, or
// the timeout fires. It always unregisters the qid before returning so a late
// answer becomes a no-op. The timer uses NewTimer + Stop (not time.After) to
// avoid leaking a timer for the full duration on the fast path.
func (s *Server) waitAsk(ctx context.Context, b BotAPI, qid string, ch chan askAnswer, options []string) *mcptypes.CallToolResult {
	defer s.unregisterAsk(qid)

	s.askMu.Lock()
	timeout := s.askTimeout
	s.askMu.Unlock()

	t := time.NewTimer(timeout)
	defer t.Stop()

	select {
	case ans := <-ch:
		if ans.err != nil {
			slog.Warn("ask cancelled while waiting", "qid", qid, "err", ans.err)
			return mcptypes.NewToolResultError("question cancelled: " + ans.err.Error())
		}

		if ans.idx < 0 || ans.idx >= len(options) {
			slog.Warn("ask answer index out of range", "qid", qid, "idx", ans.idx, "options", len(options))
			return mcptypes.NewToolResultError("received an out-of-range answer")
		}

		slog.Info("ask answered", "qid", qid, "idx", ans.idx, "user", ans.user)

		return mcptypes.NewToolResultText(fmt.Sprintf("chose: %s (index %d, by %s)", options[ans.idx], ans.idx, ans.user))
	case <-ctx.Done():
		slog.Info("ask cancelled by caller", "qid", qid)
		s.cancelAskOnDaemon(b, qid)

		return mcptypes.NewToolResultError("question cancelled before an answer arrived")
	case <-t.C:
		slog.Info("ask timed out", "qid", qid, "timeout", timeout)
		s.cancelAskOnDaemon(b, qid)

		return mcptypes.NewToolResultError("no answer within the time limit")
	}
}

// cancelAskOnDaemon tells the daemon to forget qid after the tool gave up, so
// the daemon-side askOwners entry doesn't leak and a late tap on the still-
// visible button resolves to nothing. Runs on a fresh, short-lived context so a
// cancelled caller ctx doesn't also abort the cleanup. Best-effort: on the
// disconnect path the daemon is already gone (CancelAllAsks handled the wait),
// so an error here is expected and only logged.
func (s *Server) cancelAskOnDaemon(b BotAPI, qid string) {
	cctx, cancel := context.WithTimeout(context.Background(), askCancelTimeout)
	defer cancel()

	if err := b.CancelAsk(cctx, qid); err != nil {
		slog.Info("ask cancel notify failed (daemon may be gone)", "qid", qid, "err", err)
	}
}

// validateAsk enforces the input bounds (AC10). Returns a human error string,
// or "" when valid. Kept separate so handleAsk stays readable and the bounds
// are unit-testable in isolation.
func (s *Server) validateAsk(question string, options []string) string {
	s.askMu.Lock()
	maxOpts, maxQLen, maxLabel := s.askMaxOpts, s.askMaxQLen, s.askMaxLabel
	s.askMu.Unlock()

	if len(question) == 0 {
		return "question must not be empty"
	}

	if len(question) > maxQLen {
		return fmt.Sprintf("question too long (%d bytes, max %d)", len(question), maxQLen)
	}

	if len(options) < askMinOptions {
		return fmt.Sprintf("need at least %d options", askMinOptions)
	}

	if len(options) > maxOpts {
		return fmt.Sprintf("too many options (%d, max %d)", len(options), maxOpts)
	}

	for i, o := range options {
		if len(o) == 0 {
			return fmt.Sprintf("option %d is empty", i)
		}

		if len(o) > maxLabel {
			return fmt.Sprintf("option %d too long (%d bytes, max %d)", i, len(o), maxLabel)
		}
	}

	return ""
}
