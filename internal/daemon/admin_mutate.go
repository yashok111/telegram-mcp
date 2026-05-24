package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/bot"
	"github.com/yakov/telegram-mcp/internal/chunk"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

// adminActor labels every audit entry. Single admin agent per daemon, so the
// per-hour rate limiter is global rather than per-user.
const adminActor = "admin"

// defaultPinTTL is used when pin_chat_to_shim omits ttl_seconds.
const defaultPinTTL = time.Hour

// mutateTier is the daemon-authoritative risk tier of a mutate tool. The value
// doubles as the wire `tier` field (2 / 3) so callers see the same number the
// daemon classified by. The agent never supplies a tier — it cannot downgrade
// a Tier-3 tool. This table is the #1 injection defense (handoff threat model).
type mutateTier int

const (
	tierUnknown mutateTier = 0
	tierLow     mutateTier = 2 // auto-apply + post-hoc report
	tierHigh    mutateTier = 3 // owner ✅/❌ confirm before apply
)

// spawnCanceller / bgCanceller are the Cancel slices of *SpawnRunner /
// *BgRunner the mutator needs. Interfaces keep the mutator testable.
type (
	spawnCanceller interface{ Cancel(id string) error }
	bgCanceller    interface{ Cancel(id string) error }
)

// mutateSpec binds a tool to its tier, request-time validator (summarize), and
// applier. summarize must reject anything that should not even reach an owner
// confirm (unknown target, admin evict, owner lockout); apply re-validates at
// execution time because state can change between confirm and tap.
type mutateSpec struct {
	tier      mutateTier
	summarize func(*AdminMutator, json.RawMessage) (string, error)
	apply     func(*AdminMutator, context.Context, json.RawMessage) (string, error)
}

var mutateRegistry = map[string]mutateSpec{
	"label_session":     {tierLow, (*AdminMutator).sumLabel, (*AdminMutator).applyLabel},
	"pin_chat_to_shim":  {tierLow, (*AdminMutator).sumPin, (*AdminMutator).applyPin},
	"unpin_chat":        {tierLow, (*AdminMutator).sumUnpin, (*AdminMutator).applyUnpin},
	"cancel_spawn":      {tierLow, (*AdminMutator).sumCancelSpawn, (*AdminMutator).applyCancelSpawn},
	"cancel_bg":         {tierLow, (*AdminMutator).sumCancelBg, (*AdminMutator).applyCancelBg},
	"set_effort":        {tierLow, (*AdminMutator).sumSetEffort, (*AdminMutator).applySetEffort},
	"evict_session":     {tierHigh, (*AdminMutator).sumEvict, (*AdminMutator).applyEvict},
	"approve_pairing":   {tierHigh, (*AdminMutator).sumApprovePairing, (*AdminMutator).applyApprovePairing},
	"deny_pairing":      {tierHigh, (*AdminMutator).sumDenyPairing, (*AdminMutator).applyDenyPairing},
	"add_allow":         {tierHigh, (*AdminMutator).sumAddAllow, (*AdminMutator).applyAddAllow},
	"remove_allow":      {tierHigh, (*AdminMutator).sumRemoveAllow, (*AdminMutator).applyRemoveAllow},
	"add_rule":          {tierHigh, (*AdminMutator).sumAddRule, (*AdminMutator).applyAddRule},
	"revoke_rule":       {tierHigh, (*AdminMutator).sumRevokeRule, (*AdminMutator).applyRevokeRule},
	"broadcast_message": {tierHigh, (*AdminMutator).sumBroadcast, (*AdminMutator).applyBroadcast},
}

// HandleAdminMutate is the admin.mutate IPC handler. Token-gated per-call
// (constant-time, mirrors admin.snapshot); the caller never does hello so it is
// not a routable shim. Delegates to the AdminMutator, which owns the
// authoritative tier table — a caller cannot claim or forge a tier.
func (h *Handlers) HandleAdminMutate(ctx context.Context, _ *ipc.Conn, params json.RawMessage) (any, *ipc.Error) {
	var p struct {
		Token string          `json:"token"`
		Tool  string          `json:"tool"`
		Args  json.RawMessage `json:"args"`
	}

	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ipc.Error{Code: ipc.CodeInvalidParams, Message: "bad admin mutate params"}
	}

	if !h.adminTokenValid(p.Token) {
		return nil, &ipc.Error{Code: ipc.CodeUnauthorized, Message: "admin token required"}
	}

	if h.mutator == nil {
		return nil, &ipc.Error{Code: ipc.CodeMutateRejected, Message: "admin mutations not enabled on this daemon"}
	}

	res, rpcErr := h.mutator.Mutate(ctx, p.Tool, p.Args)
	if rpcErr != nil {
		return nil, rpcErr
	}

	return res, nil
}

// classifyTier returns the daemon's authoritative tier for tool, tierUnknown if
// the tool is not in the registry.
func classifyTier(tool string) mutateTier {
	if spec, ok := mutateRegistry[tool]; ok {
		return spec.tier
	}

	return tierUnknown
}

// AdminMutateResult is the admin.mutate reply. Tier is the daemon-classified
// tier (2/3). Applied=true means a Tier-2 mutation ran now; Pending=true means a
// Tier-3 mutation is awaiting the owner's tap (PendingID identifies it).
type AdminMutateResult struct {
	Tool      string `json:"tool"`
	Tier      int    `json:"tier"`
	Applied   bool   `json:"applied"`
	Pending   bool   `json:"pending"`
	PendingID string `json:"pending_id,omitempty"`
	Result    string `json:"result"`
}

// AdminMutateConfig wires the mutator's collaborators. Spawns/Bgs may be nil
// (cancel tools then error). RatePerHour <= 0 → 60; PendingTTL <= 0 → 5m.
type AdminMutateConfig struct {
	Store       *access.Store
	Router      *Router
	Bot         botSurface
	Spawns      spawnCanceller
	Bgs         bgCanceller
	Pending     *PendingStore
	Audit       *AdminAudit
	Denylist    []string
	RatePerHour int
	PendingTTL  time.Duration
}

// AdminMutator applies admin-tools mutations daemon-side. Tier-2 tools apply
// immediately and post-hoc report; Tier-3 tools register a pending mutation and
// render an owner ✅/❌ confirm (resolved later via Resolve). The daemon — not
// the agent — owns the tier table, denylist, and rate limit.
type AdminMutator struct {
	store    *access.Store
	router   *Router
	bot      botSurface
	spawns   spawnCanceller
	bgs      bgCanceller
	pending  *PendingStore
	audit    *AdminAudit
	denylist map[string]bool

	ratePerHour int
	pendingTTL  time.Duration

	mu         sync.Mutex
	rateStamps []time.Time
}

// NewAdminMutator builds a mutator, applying defaults for rate / TTL and
// turning the denylist slice into a lookup set.
func NewAdminMutator(cfg AdminMutateConfig) *AdminMutator {
	if cfg.RatePerHour <= 0 {
		cfg.RatePerHour = 60
	}

	if cfg.PendingTTL <= 0 {
		cfg.PendingTTL = 5 * time.Minute
	}

	deny := make(map[string]bool, len(cfg.Denylist))
	for _, t := range cfg.Denylist {
		if t = strings.TrimSpace(t); t != "" {
			deny[t] = true
		}
	}

	return &AdminMutator{
		store:       cfg.Store,
		router:      cfg.Router,
		bot:         cfg.Bot,
		spawns:      cfg.Spawns,
		bgs:         cfg.Bgs,
		pending:     cfg.Pending,
		audit:       cfg.Audit,
		denylist:    deny,
		ratePerHour: cfg.RatePerHour,
		pendingTTL:  cfg.PendingTTL,
	}
}

// Mutate is the admin.mutate entry point. Order (handoff flow): denylist →
// classify → rate-limit → validate → tier branch. Tier-2 applies now; Tier-3
// registers a pending confirm. Every decision is audited.
func (m *AdminMutator) Mutate(ctx context.Context, tool string, rawArgs json.RawMessage) (AdminMutateResult, *ipc.Error) {
	if m.denylist[tool] {
		m.audit.Log("denied", tool, "", adminActor, "blocked by denylist")
		return AdminMutateResult{}, mutateErr("tool %q is disabled by the operator denylist", tool)
	}

	tier := classifyTier(tool)
	if tier == tierUnknown {
		m.audit.Log("denied", tool, "", adminActor, "unknown tool")
		return AdminMutateResult{}, mutateErr("unknown mutate tool: %q", tool)
	}

	if !m.allowRate() {
		m.audit.Log("denied", tool, "", adminActor, "rate limited")
		return AdminMutateResult{}, mutateErr("admin mutate rate limit exceeded (%d/hour) — try again later", m.ratePerHour)
	}

	summary, err := m.summarize(tool, rawArgs)
	if err != nil {
		m.audit.Log("denied", tool, "", adminActor, err.Error())
		return AdminMutateResult{}, mutateErr("%s", err.Error())
	}

	m.audit.Log("requested", tool, summary, adminActor, "")

	if tier == tierLow {
		return m.applyTier2(ctx, tool, rawArgs, summary)
	}

	return m.requestConfirm(ctx, tool, rawArgs, summary)
}

// applyTier2 runs a low-risk mutation immediately, audits the outcome, and
// fires the post-hoc owner DM.
func (m *AdminMutator) applyTier2(ctx context.Context, tool string, rawArgs json.RawMessage, summary string) (AdminMutateResult, *ipc.Error) {
	result, err := m.applyMutation(ctx, tool, rawArgs)
	if err != nil {
		m.audit.Log("failed", tool, summary, adminActor, err.Error())
		return AdminMutateResult{}, mutateErr("%s", err.Error())
	}

	m.audit.Log("applied", tool, result, adminActor, "ok")
	m.reportTier2(ctx, result)

	return AdminMutateResult{Tool: tool, Tier: int(tierLow), Applied: true, Result: result}, nil
}

// requestConfirm registers a Tier-3 mutation and renders the owner ✅/❌ prompt.
// Rejects when no owner DM target exists (handoff gotcha #6: never silently
// drop a high-risk mutation with no one to confirm it).
func (m *AdminMutator) requestConfirm(ctx context.Context, tool string, rawArgs json.RawMessage, summary string) (AdminMutateResult, *ipc.Error) {
	target, ok := m.ownerTarget()
	if !ok {
		m.audit.Log("denied", tool, summary, adminActor, "no owner DM target to confirm to")
		return AdminMutateResult{}, mutateErr("cannot request confirmation: no parseable chat in the allowlist to confirm to")
	}

	// Freeze volatile references (alias→shim_id, pairing code→sender) so the
	// mutation applied on the owner's tap matches exactly what the summary
	// described, even if state shifts during the confirm window.
	boundArgs, berr := m.freezeVolatileArgs(tool, rawArgs)
	if berr != nil {
		m.audit.Log("denied", tool, summary, adminActor, "bind failed: "+berr.Error())
		return AdminMutateResult{}, mutateErr("%s", berr.Error())
	}

	id, err := newPendingID()
	if err != nil {
		return AdminMutateResult{}, &ipc.Error{Code: ipc.CodeInternal, Message: "generate pending id: " + err.Error()}
	}

	// Persist the pending record BEFORE rendering the tappable confirm: a button
	// in Telegram must always have a backing record to resolve against. The
	// ConfirmMessageID is patched in after the render (it is only used by the
	// expiry sweep to edit the stale prompt).
	rec := PendingMutation{
		ID: id, Tool: tool, Args: boundArgs, Summary: summary,
		ConfirmChatID: target.ChatID, CreatedAt: time.Now().UTC(),
	}
	if perr := m.pending.Put(rec); perr != nil {
		m.audit.Log("failed", tool, summary, adminActor, "pending persist failed: "+perr.Error())
		return AdminMutateResult{}, &ipc.Error{Code: ipc.CodeInternal, Message: "persist pending: " + perr.Error()}
	}

	msgID, berr := m.bot.BroadcastMutationConfirm(ctx, target, id, summary)
	if berr != nil {
		// Render failed → no button was shown; remove the orphan record.
		_, _ = m.pending.Take(id)
		m.audit.Log("failed", tool, summary, adminActor, "confirm render failed: "+berr.Error())

		return AdminMutateResult{}, &ipc.Error{Code: ipc.CodeBotError, Message: "render confirmation: " + berr.Error()}
	}

	// Patch the rendered message id onto the record. A conditional update (not a
	// second Put): if the owner already tapped during the render, Take consumed
	// the record and we must NOT recreate it — a resurrected ghost could be
	// tapped a second time and double-apply. Only the expiry-edit target is lost.
	if perr := m.pending.SetConfirmMessageID(id, msgID); perr != nil {
		slog.Warn("pending msgid patch failed", "pending_id", id, "err", perr)
	}

	m.audit.Log("pending", tool, summary, adminActor, "awaiting owner confirmation id="+id)

	return AdminMutateResult{
		Tool: tool, Tier: int(tierHigh), Pending: true, PendingID: id,
		Result: "⏳ awaiting owner approval: " + summary,
	}, nil
}

// Resolve applies (approve) or discards (cancel) a pending Tier-3 mutation on
// the owner's tap. Take consumes the pending so a mutation resolves at most
// once. Returns (applied, detail) for the bot to render: applied=true only when
// a mutation actually executed. A missing id (expired/swept/already-resolved)
// or an apply failure both return applied=false with an explanatory detail.
func (m *AdminMutator) Resolve(ctx context.Context, pendingID string, approve bool) (bool, string) {
	p, ok := m.pending.Take(pendingID)
	if !ok {
		return false, "this request is no longer available (expired or already handled)"
	}

	if !approve {
		m.audit.Log("cancelled", p.Tool, p.Summary, adminActor, "owner declined")
		return false, "cancelled: " + p.Summary
	}

	result, err := m.applyMutation(ctx, p.Tool, p.Args)
	if err != nil {
		m.audit.Log("failed", p.Tool, p.Summary, adminActor, "apply on confirm failed: "+err.Error())
		return false, "failed to apply: " + err.Error()
	}

	m.audit.Log("confirmed", p.Tool, result, adminActor, "owner approved")

	return true, result
}

// applyMutation dispatches to the tool's applier. Shared by the Tier-2 path
// (called immediately) and the Tier-3 path (called on the owner tap, see
// Resolve). Re-validates via the applier even though summarize already ran.
func (m *AdminMutator) applyMutation(ctx context.Context, tool string, rawArgs json.RawMessage) (string, error) {
	spec, ok := mutateRegistry[tool]
	if !ok {
		return "", fmt.Errorf("unknown mutate tool: %s", tool)
	}

	return spec.apply(m, ctx, rawArgs)
}

// summarize validates args and renders the human-readable mutation description
// used for the confirm prompt + audit. Errors here reject the request before it
// reaches an owner confirm.
func (m *AdminMutator) summarize(tool string, rawArgs json.RawMessage) (string, error) {
	spec, ok := mutateRegistry[tool]
	if !ok {
		return "", fmt.Errorf("unknown mutate tool: %s", tool)
	}

	return spec.summarize(m, rawArgs)
}

// reportTier2 DMs the owner a post-hoc summary of an applied Tier-2 mutation.
// Best-effort: a failed report is logged, never fatal (the mutation is done).
func (m *AdminMutator) reportTier2(ctx context.Context, result string) {
	target, ok := m.ownerTarget()
	if !ok {
		return
	}

	opts := bot.SendOpts{}
	if target.ThreadID > 0 {
		opts.MessageThreadID = target.ThreadID
	}

	if _, err := m.bot.SendMessage(ctx, strconv.FormatInt(target.ChatID, 10), "🛠 admin (auto): "+result, opts); err != nil {
		slog.Warn("tier-2 post-hoc report failed", "err", err)
	}
}

// Run sweeps expired pending mutations every minute until ctx is done. Added to
// startBackgroundWorkers. Nil-receiver-safe so a daemon with no mutator skips it.
func (m *AdminMutator) Run(ctx context.Context) {
	if m == nil {
		return
	}

	t := time.NewTicker(time.Minute)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweepExpired(ctx)
		}
	}
}

// sweepExpired drops pending mutations past TTL, audits each as expired, and
// edits its confirm message to mark it stale (removing the now-dead buttons).
func (m *AdminMutator) sweepExpired(ctx context.Context) {
	for _, p := range m.pending.Sweep(m.pendingTTL) {
		m.audit.Log("expired", p.Tool, p.Summary, adminActor, "ttl elapsed without owner action")

		if p.ConfirmChatID == 0 || p.ConfirmMessageID == 0 {
			continue
		}

		if _, err := m.bot.EditMessage(ctx, strconv.FormatInt(p.ConfirmChatID, 10), p.ConfirmMessageID,
			"⌛ Expired (no response): "+p.Summary, ""); err != nil {
			slog.Warn("expired confirm edit failed", "pending_id", p.ID, "err", err)
		}
	}
}

// ownerTarget resolves the owner's DM as the first parseable allowlist entry —
// the same single-user assumption pickPermissionTarget uses. ThreadID is always
// 0: admin confirms/reports go to the owner's DM, not a forum topic.
func (m *AdminMutator) ownerTarget() (bot.PermissionTarget, bool) {
	if id, ok := firstParseableChatID(m.store.Load().AllowFrom); ok {
		return bot.PermissionTarget{ChatID: id}, true
	}

	return bot.PermissionTarget{}, false
}

// firstParseableChatID returns the first allowlist entry that parses as a
// POSITIVE int64 — the operator's owner DM under the single-user model. A DM
// chat id is always positive; a group/channel id is negative and must never be
// taken as the owner (admin confirms/reports would go to a group, and the
// owner-only tap check would misfire). Shared by ownerTarget and the atomic
// owner-lockout check so both agree on "the owner".
func firstParseableChatID(allowFrom []string) (int64, bool) {
	for _, raw := range allowFrom {
		if id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && id > 0 {
			return id, true
		}
	}

	return 0, false
}

// ownerChatID is ownerTarget rendered as the canonical chat-id string, "" when
// none parses. Used by the owner-lockout guard.
func (m *AdminMutator) ownerChatID() string {
	if t, ok := m.ownerTarget(); ok {
		return strconv.FormatInt(t.ChatID, 10)
	}

	return ""
}

// allowRate enforces the global per-hour mutate cap with a stamp slice (single
// admin, so no per-user keying needed).
func (m *AdminMutator) allowRate() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-time.Hour)

	// In-place filter (same pattern as spawn.go reserveSlot): correct because we
	// hold m.mu and the range reads the slice header before any overwrite.
	keep := m.rateStamps[:0]
	for _, t := range m.rateStamps {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}

	m.rateStamps = keep

	if len(m.rateStamps) >= m.ratePerHour {
		return false
	}

	m.rateStamps = append(m.rateStamps, time.Now())

	return true
}

func mutateErr(format string, a ...any) *ipc.Error {
	return &ipc.Error{Code: ipc.CodeMutateRejected, Message: fmt.Sprintf(format, a...)}
}

func newPendingID() (string, error) {
	// 8 bytes → 16 hex chars. The id rides in callback data (am:<id>:confirm,
	// well under Telegram's 64-byte cap) and the only authorization is the
	// allowlist gate on the tapper, so make the id itself unguessable.
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}

	return id
}

// canonChatID parses a chat_id arg and returns it in canonical decimal form
// (matching how Telegram ids are stored throughout). Rejects non-numeric input.
// Canonicalizing closes the owner-lockout bypass where " 330621952" or a
// leading-zero variant would compare unequal to the stored owner id.
func canonChatID(s string) (string, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid chat_id %q", s)
	}

	return strconv.FormatInt(id, 10), nil
}

// --- argument shapes ---

type (
	evictArgs struct {
		Target string `json:"target"`
		// ShimID is frozen onto the pending record at confirm time so apply
		// evicts the exact shim the owner saw, not whatever currently holds the
		// alias (aliases are reused as shims reconnect).
		ShimID string `json:"shim_id,omitempty"`
	}
	labelArgs struct {
		Target string `json:"target"`
		Label  string `json:"label"`
	}
	pinArgs struct {
		ChatID     string `json:"chat_id"`
		Target     string `json:"target"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	chatArgs struct {
		ChatID string `json:"chat_id"`
	}
	idArgs struct {
		ID string `json:"id"`
	}
	effortArgs struct {
		ChatID string `json:"chat_id"`
		Level  string `json:"level"`
	}
	codeArgs struct {
		Code string `json:"code"`
		// SenderID is frozen onto the pending record at confirm time so apply
		// refuses if the pairing code was reused for a different sender between
		// the owner's view and their tap.
		SenderID string `json:"sender_id,omitempty"`
	}
	ruleArgs struct {
		Tool        string `json:"tool"`
		Action      string `json:"action"`
		PathPattern string `json:"path_pattern"`
		TTLSeconds  int    `json:"ttl_seconds"`
	}
	textArgs struct {
		Text string `json:"text"`
	}
)

func decodeMutateArgs[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, errors.New("missing arguments")
	}

	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}

	return v, nil
}

// --- label_session (Tier-2) ---

func (m *AdminMutator) sumLabel(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[labelArgs](raw)
	if err != nil {
		return "", err
	}

	sh, err := m.router.ResolveShim(a.Target)
	if err != nil {
		return "", fmt.Errorf("label_session target %q: %w", a.Target, err)
	}

	return fmt.Sprintf("label session %s (%s) as %q", sh.Alias, shortID(sh.ID), a.Label), nil
}

func (m *AdminMutator) applyLabel(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[labelArgs](raw)
	if err != nil {
		return "", err
	}

	sh, err := m.router.ResolveShim(a.Target)
	if err != nil {
		return "", fmt.Errorf("label_session target %q: %w", a.Target, err)
	}

	if _, err := m.router.SetLabel(sh.ID, a.Label); err != nil {
		return "", fmt.Errorf("set label: %w", err)
	}

	return fmt.Sprintf("labelled %s (%s) as %q", sh.Alias, shortID(sh.ID), a.Label), nil
}

// --- pin_chat_to_shim (Tier-2) ---

func (m *AdminMutator) sumPin(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[pinArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("pin_chat_to_shim: %w", err)
	}

	sh, err := m.router.ResolveShim(a.Target)
	if err != nil {
		return "", fmt.Errorf("pin_chat_to_shim target %q: %w", a.Target, err)
	}

	return fmt.Sprintf("pin chat %s to %s (%s) for %s", cid, sh.Alias, shortID(sh.ID), pinTTL(a.TTLSeconds)), nil
}

func (m *AdminMutator) applyPin(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[pinArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("pin_chat_to_shim: %w", err)
	}

	sh, err := m.router.ResolveShim(a.Target)
	if err != nil {
		return "", fmt.Errorf("pin_chat_to_shim target %q: %w", a.Target, err)
	}

	if err := m.router.Pin(cid, sh.ID, pinTTL(a.TTLSeconds)); err != nil {
		return "", fmt.Errorf("pin: %w", err)
	}

	return fmt.Sprintf("pinned chat %s to %s (%s) for %s", cid, sh.Alias, shortID(sh.ID), pinTTL(a.TTLSeconds)), nil
}

// maxTTLSeconds caps a caller-supplied TTL so time.Duration(secs)*time.Second
// can't overflow int64 and wrap to a negative (past) expiry. 10 years is well
// beyond any legitimate pin/rule lifetime.
const maxTTLSeconds = 10 * 365 * 24 * 3600

func clampTTLSeconds(secs int) int {
	if secs > maxTTLSeconds {
		return maxTTLSeconds
	}

	return secs
}

func pinTTL(secs int) time.Duration {
	if secs <= 0 {
		return defaultPinTTL
	}

	return time.Duration(clampTTLSeconds(secs)) * time.Second
}

// --- unpin_chat (Tier-2) ---

func (m *AdminMutator) sumUnpin(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[chatArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("unpin_chat: %w", err)
	}

	return "unpin chat " + cid, nil
}

func (m *AdminMutator) applyUnpin(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[chatArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("unpin_chat: %w", err)
	}

	m.router.Unpin(cid)

	return "unpinned chat " + cid, nil
}

// --- cancel_spawn (Tier-2) ---

func (m *AdminMutator) sumCancelSpawn(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[idArgs](raw)
	if err != nil {
		return "", err
	}

	if a.ID == "" {
		return "", errors.New("cancel_spawn: id required")
	}

	return "cancel spawn " + a.ID, nil
}

func (m *AdminMutator) applyCancelSpawn(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[idArgs](raw)
	if err != nil {
		return "", err
	}

	if a.ID == "" {
		return "", errors.New("cancel_spawn: id required")
	}

	if m.spawns == nil {
		return "", errors.New("cancel_spawn: spawn manager not configured")
	}

	if err := m.spawns.Cancel(a.ID); err != nil {
		return "", fmt.Errorf("cancel spawn: %w", err)
	}

	return "cancelled spawn " + a.ID, nil
}

// --- cancel_bg (Tier-2) ---

func (m *AdminMutator) sumCancelBg(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[idArgs](raw)
	if err != nil {
		return "", err
	}

	if a.ID == "" {
		return "", errors.New("cancel_bg: id required")
	}

	return "cancel bg task " + a.ID, nil
}

func (m *AdminMutator) applyCancelBg(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[idArgs](raw)
	if err != nil {
		return "", err
	}

	if a.ID == "" {
		return "", errors.New("cancel_bg: id required")
	}

	if m.bgs == nil {
		return "", errors.New("cancel_bg: bg manager not configured")
	}

	if err := m.bgs.Cancel(a.ID); err != nil {
		return "", fmt.Errorf("cancel bg: %w", err)
	}

	return "cancelled bg task " + a.ID, nil
}

// --- set_effort (Tier-2) ---

func (m *AdminMutator) sumSetEffort(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[effortArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("set_effort: %w", err)
	}

	if isEffortClear(a.Level) {
		return "clear effort override for chat " + cid, nil
	}

	if _, ok := bot.ResolveEffort(a.Level); !ok {
		return "", fmt.Errorf("set_effort: unknown level %q (use low|medium|high|xhigh|max|clear)", a.Level)
	}

	return fmt.Sprintf("set effort for chat %s to %s", cid, a.Level), nil
}

func (m *AdminMutator) applySetEffort(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[effortArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("set_effort: %w", err)
	}

	if isEffortClear(a.Level) {
		if serr := m.store.Mutate(func(st *access.State) bool {
			if st.EffortByChat == nil {
				return false
			}

			if _, ok := st.EffortByChat[cid]; !ok {
				return false
			}

			delete(st.EffortByChat, cid)

			return true
		}); serr != nil {
			return "", fmt.Errorf("save: %w", serr)
		}

		return "cleared effort override for chat " + cid, nil
	}

	if _, ok := bot.ResolveEffort(a.Level); !ok {
		return "", fmt.Errorf("set_effort: unknown level %q", a.Level)
	}

	if serr := m.store.Mutate(func(st *access.State) bool {
		if st.EffortByChat == nil {
			st.EffortByChat = map[string]string{}
		}

		st.EffortByChat[cid] = strings.ToLower(strings.TrimSpace(a.Level))

		return true
	}); serr != nil {
		return "", fmt.Errorf("save: %w", serr)
	}

	return fmt.Sprintf("set effort for chat %s to %s", cid, a.Level), nil
}

func isEffortClear(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "clear", "off", "reset":
		return true
	}

	return false
}

// --- evict_session (Tier-3) ---

// resolveEvictTarget resolves the target shim and refuses the admin session.
// AC6: evict_session @admin is blocked and the admin can never be evicted —
// checked at both summarize (request) and apply (tap) time.
func (m *AdminMutator) resolveEvictTarget(target string) (*Shim, error) {
	sh, err := m.router.ResolveShim(target)
	if err != nil {
		return nil, fmt.Errorf("evict_session target %q: %w", target, err)
	}

	if sh.Role == "admin" {
		return nil, errors.New("evict_session: refusing to evict the admin session")
	}

	return sh, nil
}

func (m *AdminMutator) sumEvict(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[evictArgs](raw)
	if err != nil {
		return "", err
	}

	sh, err := m.resolveEvictTarget(a.Target)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("evict session %s (%s, workdir %s) — sends shutdown", sh.Alias, shortID(sh.ID), sh.Workdir), nil
}

// bindEvict freezes the resolved shim_id onto the args so applyEvict targets the
// exact session the owner confirmed, not whatever holds the alias at tap time.
func (m *AdminMutator) bindEvict(raw json.RawMessage) (json.RawMessage, error) {
	a, err := decodeMutateArgs[evictArgs](raw)
	if err != nil {
		return nil, err
	}

	sh, err := m.resolveEvictTarget(a.Target)
	if err != nil {
		return nil, err
	}

	return json.Marshal(evictArgs{Target: a.Target, ShimID: sh.ID})
}

func (m *AdminMutator) applyEvict(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[evictArgs](raw)
	if err != nil {
		return "", err
	}

	// Prefer the shim_id frozen at confirm time (immune to alias reuse); fall
	// back to the raw target for any record created before binding existed.
	ref := a.Target
	if a.ShimID != "" {
		ref = a.ShimID
	}

	sh, err := m.resolveEvictTarget(ref)
	if err != nil {
		return "", err
	}

	if sh.Notify == nil {
		return "", errors.New("evict_session: target has no notify channel (already gone)")
	}

	// Shutdown notify, not Drop: Drop just forgets the shim, leaving the process
	// running (handoff gotcha #3). NotifyShutdown asks the shim to exit.
	if nerr := sh.Notify(ipc.NotifyShutdown, struct{}{}); nerr != nil {
		return "", fmt.Errorf("evict notify: %w", nerr)
	}

	return fmt.Sprintf("evicted %s (%s) — shutdown sent", sh.Alias, shortID(sh.ID)), nil
}

// --- approve_pairing / deny_pairing (Tier-3) ---

func (m *AdminMutator) sumApprovePairing(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[codeArgs](raw)
	if err != nil {
		return "", err
	}

	p, ok := m.lookupPending(a.Code)
	if !ok {
		return "", fmt.Errorf("approve_pairing: code %q is not pending", a.Code)
	}

	return fmt.Sprintf("approve pairing %s → add sender %s (chat %s) to allowlist", a.Code, p.SenderID, p.ChatID), nil
}

// bindApprovePairing freezes the resolved sender onto the args so apply can
// refuse if the pairing code is reused for a different sender before the tap.
func (m *AdminMutator) bindApprovePairing(raw json.RawMessage) (json.RawMessage, error) {
	a, err := decodeMutateArgs[codeArgs](raw)
	if err != nil {
		return nil, err
	}

	p, ok := m.lookupPending(a.Code)
	if !ok {
		return nil, fmt.Errorf("approve_pairing: code %q is not pending", a.Code)
	}

	return json.Marshal(codeArgs{Code: a.Code, SenderID: p.SenderID})
}

// freezeVolatileArgs rewrites a Tier-3 tool's args at confirm time so the later
// apply operates on a stable reference. Tools whose args point at state that can
// change during the confirm window (an alias that may be reassigned, a pairing
// code whose sender may be replaced) resolve to a frozen id here; all others
// pass through unchanged.
func (m *AdminMutator) freezeVolatileArgs(tool string, raw json.RawMessage) (json.RawMessage, error) {
	switch tool {
	case "evict_session":
		return m.bindEvict(raw)
	case "approve_pairing":
		return m.bindApprovePairing(raw)
	default:
		return raw, nil
	}
}

func (m *AdminMutator) applyApprovePairing(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[codeArgs](raw)
	if err != nil {
		return "", err
	}

	var (
		notFound      bool
		badSender     bool
		senderChanged bool
		sender        string
	)

	if serr := m.store.Mutate(func(st *access.State) bool {
		p, ok := st.Pending[a.Code]
		if !ok || pairingExpired(p) {
			notFound = true
			return false
		}

		// If the confirm froze a sender, refuse when the live pending now
		// resolves to a different one (code reused after expiry) — the owner only
		// ever approves the sender they saw.
		if a.SenderID != "" && !sameChatID(p.SenderID, a.SenderID) {
			senderChanged = true
			return false
		}

		// Canonicalize the sender id so it matches the form the lockout guard
		// compares against; refuse to approve a malformed sender rather than
		// silently dropping it from the allowlist.
		cid, cerr := canonChatID(p.SenderID)
		if cerr != nil {
			badSender = true
			return false
		}

		sender = cid
		if !slices.Contains(st.AllowFrom, cid) {
			st.AllowFrom = append(st.AllowFrom, cid)
		}

		delete(st.Pending, a.Code)

		return true
	}); serr != nil {
		return "", fmt.Errorf("save: %w", serr)
	}

	if badSender {
		return "", fmt.Errorf("approve_pairing: code %q has a malformed sender id", a.Code)
	}

	if senderChanged {
		return "", fmt.Errorf("approve_pairing: code %q now resolves to a different sender than confirmed; not approving", a.Code)
	}

	if notFound {
		return "", fmt.Errorf("approve_pairing: code %q no longer pending", a.Code)
	}

	return fmt.Sprintf("approved pairing %s — added sender %s to allowlist", a.Code, sender), nil
}

// sameChatID reports whether two chat-id strings denote the same id once
// canonicalized (trims whitespace / leading zeros). Malformed input is never
// "same".
func sameChatID(a, b string) bool {
	ca, ea := canonChatID(a)
	cb, eb := canonChatID(b)

	return ea == nil && eb == nil && ca == cb
}

func (m *AdminMutator) sumDenyPairing(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[codeArgs](raw)
	if err != nil {
		return "", err
	}

	p, ok := m.lookupPending(a.Code)
	if !ok {
		return "", fmt.Errorf("deny_pairing: code %q is not pending", a.Code)
	}

	return fmt.Sprintf("deny pairing %s (sender %s)", a.Code, p.SenderID), nil
}

func (m *AdminMutator) applyDenyPairing(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[codeArgs](raw)
	if err != nil {
		return "", err
	}

	var notFound bool

	if serr := m.store.Mutate(func(st *access.State) bool {
		if _, ok := st.Pending[a.Code]; !ok {
			notFound = true
			return false
		}

		delete(st.Pending, a.Code)

		return true
	}); serr != nil {
		return "", fmt.Errorf("save: %w", serr)
	}

	if notFound {
		return "", fmt.Errorf("deny_pairing: code %q no longer pending", a.Code)
	}

	return "denied pairing " + a.Code, nil
}

func (m *AdminMutator) lookupPending(code string) (access.Pending, bool) {
	p, ok := m.store.Load().Pending[code]
	if !ok || pairingExpired(p) {
		return access.Pending{}, false
	}

	return p, true
}

// pairingExpired reports whether a pairing entry has aged out. The daemon's
// RulesCleanup ticker only prunes once a minute, so the admin path must skip
// an expired code itself rather than approve a stale requester in that window.
func pairingExpired(p access.Pending) bool {
	return p.ExpiresAt > 0 && time.Now().UnixMilli() > p.ExpiresAt
}

// --- add_allow / remove_allow (Tier-3) ---

func (m *AdminMutator) sumAddAllow(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[chatArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("add_allow: %w", err)
	}

	return "add chat " + cid + " to allowlist", nil
}

func (m *AdminMutator) applyAddAllow(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[chatArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := canonChatID(a.ChatID)
	if err != nil {
		return "", fmt.Errorf("add_allow: %w", err)
	}

	if serr := m.store.Mutate(func(st *access.State) bool {
		if slices.Contains(st.AllowFrom, cid) {
			return false
		}

		st.AllowFrom = append(st.AllowFrom, cid)

		return true
	}); serr != nil {
		return "", fmt.Errorf("save: %w", serr)
	}

	return "added chat " + cid + " to allowlist", nil
}

func (m *AdminMutator) sumRemoveAllow(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[chatArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := m.checkRemoveAllow(a.ChatID)
	if err != nil {
		return "", err
	}

	return "remove chat " + cid + " from allowlist", nil
}

func (m *AdminMutator) applyRemoveAllow(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[chatArgs](raw)
	if err != nil {
		return "", err
	}

	cid, err := m.checkRemoveAllow(a.ChatID)
	if err != nil {
		return "", err
	}

	var (
		removed bool
		lockout bool
	)

	if serr := m.store.Mutate(func(st *access.State) bool {
		// Re-check the owner against the same state we're about to mutate — the
		// pre-load check in checkRemoveAllow is racy if AllowFrom shifts between
		// the load and this closure.
		if ownerID, ok := firstParseableChatID(st.AllowFrom); ok && strconv.FormatInt(ownerID, 10) == cid {
			lockout = true
			return false
		}

		idx := slices.Index(st.AllowFrom, cid)
		if idx < 0 {
			return false
		}

		st.AllowFrom = slices.Delete(st.AllowFrom, idx, idx+1)
		removed = true

		return true
	}); serr != nil {
		return "", fmt.Errorf("save: %w", serr)
	}

	if lockout {
		return "", errors.New("remove_allow: refusing to remove the owner's own chat (lockout guard)")
	}

	if !removed {
		return "", fmt.Errorf("remove_allow: chat %s not in allowlist", cid)
	}

	return "removed chat " + cid + " from allowlist", nil
}

// checkRemoveAllow canonicalizes the chat_id and enforces the owner-lockout
// guard (AC6 / handoff gotcha #4): the operator's own DM chat (the first
// parseable allowlist entry) may never be removed, or they lose all access.
// Comparing canonical forms closes the " 330621952" / leading-zero bypass.
func (m *AdminMutator) checkRemoveAllow(chatID string) (string, error) {
	cid, err := canonChatID(chatID)
	if err != nil {
		return "", fmt.Errorf("remove_allow: %w", err)
	}

	if cid == m.ownerChatID() {
		return "", errors.New("remove_allow: refusing to remove the owner's own chat (lockout guard)")
	}

	return cid, nil
}

// --- add_rule / revoke_rule (Tier-3) ---

func (m *AdminMutator) sumAddRule(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[ruleArgs](raw)
	if err != nil {
		return "", err
	}

	if err := validateRuleArgs(a); err != nil {
		return "", err
	}

	return fmt.Sprintf("add permission rule: %s tool=%s path=%q ttl=%s", a.Action, a.Tool, a.PathPattern, ruleTTL(a.TTLSeconds)), nil
}

func (m *AdminMutator) applyAddRule(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[ruleArgs](raw)
	if err != nil {
		return "", err
	}

	if err := validateRuleArgs(a); err != nil {
		return "", err
	}

	rule := access.PermissionRule{
		ID:          access.NewRuleID(),
		Tool:        a.Tool,
		PathPattern: a.PathPattern,
		Action:      access.RuleAction(a.Action),
	}
	if a.TTLSeconds > 0 {
		rule.ExpiresAt = time.Now().Add(time.Duration(clampTTLSeconds(a.TTLSeconds)) * time.Second).UnixMilli()
	}

	if serr := m.store.Mutate(func(st *access.State) bool {
		access.AddRule(st, rule)
		return true
	}); serr != nil {
		return "", fmt.Errorf("save: %w", serr)
	}

	// Return the id so the agent can revoke_rule it without a list_rules round-trip.
	return fmt.Sprintf("added rule id=%s: %s tool=%s path=%q", rule.ID, a.Action, a.Tool, a.PathPattern), nil
}

func validateRuleArgs(a ruleArgs) error {
	if strings.TrimSpace(a.Tool) == "" {
		return errors.New("add_rule: tool required")
	}

	switch access.RuleAction(a.Action) {
	case access.RuleApprove, access.RuleDeny:
		return nil
	default:
		return fmt.Errorf("add_rule: action must be approve|deny, got %q", a.Action)
	}
}

func ruleTTL(secs int) string {
	if secs <= 0 {
		return "permanent"
	}

	// Clamp to match applyAddRule so the confirm summary shows the TTL the owner
	// will actually get, not an overflowing value.
	return (time.Duration(clampTTLSeconds(secs)) * time.Second).String()
}

func (m *AdminMutator) sumRevokeRule(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[idArgs](raw)
	if err != nil {
		return "", err
	}

	if a.ID == "" {
		return "", errors.New("revoke_rule: id required")
	}

	return "revoke permission rule " + a.ID, nil
}

func (m *AdminMutator) applyRevokeRule(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[idArgs](raw)
	if err != nil {
		return "", err
	}

	if a.ID == "" {
		return "", errors.New("revoke_rule: id required")
	}

	var revoked bool

	if serr := m.store.Mutate(func(st *access.State) bool {
		revoked = access.RevokeRule(st, a.ID)
		return revoked
	}); serr != nil {
		return "", fmt.Errorf("save: %w", serr)
	}

	if !revoked {
		return "", fmt.Errorf("revoke_rule: no rule with id %s", a.ID)
	}

	return "revoked rule " + a.ID, nil
}

// --- broadcast_message (Tier-3) ---

func (m *AdminMutator) sumBroadcast(raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[textArgs](raw)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(a.Text) == "" {
		return "", errors.New("broadcast_message: text required")
	}

	return fmt.Sprintf("broadcast to %d allowlisted chat(s): %q", m.broadcastTargetCount(), truncateSummary(a.Text, 120)), nil
}

func (m *AdminMutator) applyBroadcast(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := decodeMutateArgs[textArgs](raw)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(a.Text) == "" {
		return "", errors.New("broadcast_message: text required")
	}

	// Chunk so a >4096-char broadcast doesn't fail wholesale at the Bot API.
	chunks := chunk.Split(a.Text, 4096, chunk.Length)

	var sent, failed int

	for _, raw := range m.store.Load().AllowFrom {
		chatID := strings.TrimSpace(raw)
		if _, perr := strconv.ParseInt(chatID, 10, 64); perr != nil {
			continue
		}

		if m.broadcastTo(ctx, chatID, chunks) {
			sent++
		} else {
			failed++
		}
	}

	// A broadcast that reached nobody is a failure, not a success — the agent
	// and the audit trail must not record it as delivered.
	if sent == 0 && failed > 0 {
		return "", fmt.Errorf("broadcast_message: delivered to 0 of %d chat(s)", failed)
	}

	return fmt.Sprintf("broadcast delivered to %d chat(s), %d failed", sent, failed), nil
}

// broadcastTo sends every chunk to one chat in order; returns false (and stops)
// if any chunk fails so the chat counts as failed rather than half-delivered.
func (m *AdminMutator) broadcastTo(ctx context.Context, chatID string, chunks []string) bool {
	for _, c := range chunks {
		if _, serr := m.bot.SendMessage(ctx, chatID, c, bot.SendOpts{}); serr != nil {
			slog.Warn("broadcast send failed", "chat_id", chatID, "err", serr)
			return false
		}
	}

	return true
}

func (m *AdminMutator) broadcastTargetCount() int {
	var n int

	for _, raw := range m.store.Load().AllowFrom {
		if _, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
			n++
		}
	}

	return n
}

func truncateSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}

	// n is a byte budget; back up to a rune boundary so a multi-byte character
	// straddling the cut never leaves a partial sequence in the audit log or the
	// owner confirm prompt.
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + "…"
}
