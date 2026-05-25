package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
	"github.com/yakov/telegram-mcp/internal/ipc"
)

func TestFirstParseableChatIDSkipsNegative(t *testing.T) {
	// A negative group/channel id must never resolve as the owner; the first
	// POSITIVE id (a DM chat) wins.
	id, ok := firstParseableChatID([]string{"-1001234567890", "330621952"})
	require.True(t, ok)
	assert.Equal(t, int64(330621952), id)

	_, ok = firstParseableChatID([]string{"-100", "nan"})
	assert.False(t, ok, "no positive id => no owner")

	_, ok = firstParseableChatID(nil)
	assert.False(t, ok)
}

func TestRuleTTLClamps(t *testing.T) {
	assert.Equal(t, "permanent", ruleTTL(0))
	assert.Equal(t, (time.Duration(maxTTLSeconds) * time.Second).String(), ruleTTL(1<<62),
		"overflowing seconds clamp instead of wrapping")
}

func TestTruncateSummaryRuneSafe(t *testing.T) {
	// "aaé": é is 2 bytes (0xC3 0xA9); a byte budget of 3 lands mid-é. The cut
	// must back up to a rune boundary, never emit a partial sequence.
	out := truncateSummary("aaé", 3)
	assert.True(t, utf8.ValidString(out), "must be valid UTF-8, got %q", out)
	assert.Equal(t, "aa…", out)

	// Within budget: returned verbatim.
	assert.Equal(t, "abc", truncateSummary("abc", 3))

	// All-multibyte input truncated at an odd byte budget stays valid.
	tr := truncateSummary(strings.Repeat("é", 100), 121)
	assert.True(t, utf8.ValidString(tr))
	assert.True(t, strings.HasSuffix(tr, "…"))
}

func TestPinTTLCapsOverflow(t *testing.T) {
	// A huge ttl_seconds must not overflow time.Duration into a negative (past)
	// expiry; it clamps to maxTTLSeconds.
	d := pinTTL(1 << 62)
	assert.Positive(t, d)
	assert.Equal(t, time.Duration(maxTTLSeconds)*time.Second, d)

	assert.Equal(t, defaultPinTTL, pinTTL(0), "non-positive falls back to default")
	assert.Equal(t, 5*time.Second, pinTTL(5), "normal value passes through")
}

type fakeCanceller struct {
	cancelled []string
	err       error
}

func (f *fakeCanceller) Cancel(id string) error {
	f.cancelled = append(f.cancelled, id)
	return f.err
}

func newMutator(t *testing.T) (*AdminMutator, *fakeBot, *Router, *access.Store, *fakeCanceller, *fakeCanceller) {
	t.Helper()

	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(access.State{
		DMPolicy:  access.PolicyAllowlist,
		AllowFrom: []string{"330621952"},
		Groups:    map[string]access.GroupPolicy{},
		Pending:   map[string]access.Pending{},
	}))

	router := NewRouter()
	fb := &fakeBot{}
	sc := &fakeCanceller{}
	bc := &fakeCanceller{}

	m := NewAdminMutator(AdminMutateConfig{
		Store: store, Router: router, Bot: fb, Spawns: sc, Bgs: bc,
		Pending: NewPendingStore(dir), Audit: NewAdminAudit(dir, 0),
		RatePerHour: 60, PendingTTL: 5 * time.Minute,
	})

	return m, fb, router, store, sc, bc
}

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	return raw
}

func regShim(r *Router, id, label string) *Shim {
	s := &Shim{ID: id, Label: label, Notify: func(string, any) error { return nil }}
	r.Register(s)

	return s
}

func TestClassifyTier(t *testing.T) {
	tier2 := []string{"label_session", "pin_chat_to_shim", "unpin_chat", "cancel_spawn", "cancel_bg", "set_effort"}
	for _, tool := range tier2 {
		assert.Equal(t, tierLow, classifyTier(tool), tool)
	}

	tier3 := []string{"evict_session", "approve_pairing", "deny_pairing", "add_allow", "remove_allow", "add_rule", "revoke_rule", "broadcast_message"}
	for _, tool := range tier3 {
		assert.Equal(t, tierHigh, classifyTier(tool), tool)
	}

	assert.Equal(t, tierUnknown, classifyTier("rm_minus_rf"))
}

func TestMutateDenylistRejectedBeforeTier(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)
	m.denylist = map[string]bool{"evict_session": true}

	_, rpcErr := m.Mutate(context.Background(), "evict_session", args(t, map[string]string{"target": "s1"}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeMutateRejected, rpcErr.Code)
	assert.Contains(t, rpcErr.Message, "denylist")
}

func TestMutateUnknownToolRejected(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)

	_, rpcErr := m.Mutate(context.Background(), "drop_database", args(t, map[string]string{}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeMutateRejected, rpcErr.Code)
	assert.Contains(t, rpcErr.Message, "unknown")
}

func TestMutateRateLimited(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)
	m.ratePerHour = 2

	// unpin_chat is Tier-2 and always succeeds (idempotent), so it exercises
	// the rate counter cleanly.
	for range 2 {
		_, rpcErr := m.Mutate(context.Background(), "unpin_chat", args(t, map[string]string{"chat_id": "42"}))
		require.Nil(t, rpcErr)
	}

	_, rpcErr := m.Mutate(context.Background(), "unpin_chat", args(t, map[string]string{"chat_id": "42"}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeMutateRejected, rpcErr.Code)
	assert.Contains(t, rpcErr.Message, "rate")
}

func TestMutateTier2LabelAppliesAndReports(t *testing.T) {
	m, fb, router, _, _, _ := newMutator(t)
	sh := regShim(router, "abc123abc123", "")

	res, rpcErr := m.Mutate(context.Background(), "label_session", args(t, map[string]any{"target": sh.Alias, "label": "build-bot"}))
	require.Nil(t, rpcErr)
	assert.True(t, res.Applied)
	assert.Equal(t, 2, res.Tier)

	// Label actually changed on the router.
	for _, info := range router.Snapshot() {
		if info.ID == sh.ID {
			assert.Equal(t, "build-bot", info.Label)
		}
	}

	// Post-hoc owner DM fired.
	assert.Equal(t, "330621952", fb.sentMessage.chatID)
	assert.Contains(t, fb.sentMessage.text, "build-bot")
}

func TestMutateTier2UnpinApplies(t *testing.T) {
	m, _, router, _, _, _ := newMutator(t)
	sh := regShim(router, "deadbeef0001", "")
	require.NoError(t, router.Pin("42", sh.ID, time.Hour))

	res, rpcErr := m.Mutate(context.Background(), "unpin_chat", args(t, map[string]string{"chat_id": "42"}))
	require.Nil(t, rpcErr)
	assert.True(t, res.Applied)

	// Pin gone → a fresh inbound for chat 42 no longer routes via pin.
	for _, info := range router.Snapshot() {
		assert.NotContains(t, info.PinnedChats, "42")
	}
}

func TestMutateTier2CancelSpawn(t *testing.T) {
	m, _, _, _, sc, _ := newMutator(t)

	res, rpcErr := m.Mutate(context.Background(), "cancel_spawn", args(t, map[string]string{"id": "sp1"}))
	require.Nil(t, rpcErr)
	assert.True(t, res.Applied)
	assert.Equal(t, []string{"sp1"}, sc.cancelled)
}

func TestMutateTier2SetEffortPersists(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)

	res, rpcErr := m.Mutate(context.Background(), "set_effort", args(t, map[string]string{"chat_id": "42", "level": "high"}))
	require.Nil(t, rpcErr)
	assert.True(t, res.Applied)
	assert.Equal(t, "high", store.Load().EffortByChat["42"])
}

func TestMutateSetEffortRejectsUnknownLevel(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)

	_, rpcErr := m.Mutate(context.Background(), "set_effort", args(t, map[string]string{"chat_id": "42", "level": "turbo"}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeMutateRejected, rpcErr.Code)
}

func TestMutateTier3EvictRendersConfirmAndPends(t *testing.T) {
	m, fb, router, _, _, _ := newMutator(t)
	sh := regShim(router, "feedface0001", "")
	fb.confirmRetID = 555

	res, rpcErr := m.Mutate(context.Background(), "evict_session", args(t, map[string]string{"target": sh.Alias}))
	require.Nil(t, rpcErr)
	assert.False(t, res.Applied, "Tier-3 never applies inline")
	assert.True(t, res.Pending)
	assert.Equal(t, 3, res.Tier)
	require.NotEmpty(t, res.PendingID)

	// Confirm was rendered to the owner.
	require.Len(t, fb.mutationConfirms, 1)
	assert.Equal(t, int64(330621952), fb.mutationConfirms[0].target.ChatID)
	assert.Contains(t, fb.mutationConfirms[0].summary, sh.Alias)

	// Pending persisted with the confirm message coordinates.
	got, ok := m.pending.Take(res.PendingID)
	require.True(t, ok)
	assert.Equal(t, "evict_session", got.Tool)
	assert.Equal(t, 555, got.ConfirmMessageID)
}

// Forged-tier defense: a Tier-3 tool can never apply inline regardless of args
// — the daemon classifies from its own table, the caller cannot claim a tier.
func TestMutateTier3NeverAppliesInline(t *testing.T) {
	m, _, router, _, _, _ := newMutator(t)
	sh := regShim(router, "aaaabbbbcccc", "")
	notified := false
	sh.Notify = func(string, any) error { notified = true; return nil }

	res, rpcErr := m.Mutate(context.Background(), "evict_session", args(t, map[string]string{"target": sh.Alias}))
	require.Nil(t, rpcErr)
	assert.True(t, res.Pending)
	assert.False(t, notified, "Tier-3 evict must NOT notify the shim until the owner taps")
}

func TestMutateEvictAdminBlocked(t *testing.T) {
	m, fb, router, _, _, _ := newMutator(t)
	router.Register(&Shim{ID: "9a9a9a9a9a9a", Role: "admin", Notify: func(string, any) error { return nil }})

	_, rpcErr := m.Mutate(context.Background(), "evict_session", args(t, map[string]string{"target": AdminAlias}))
	require.NotNil(t, rpcErr, "evicting the admin session must be refused")
	assert.Equal(t, ipc.CodeMutateRejected, rpcErr.Code)
	assert.Empty(t, fb.mutationConfirms, "no confirm should be rendered for a blocked evict")
}

func TestMutateRemoveAllowOwnerLockoutBlocked(t *testing.T) {
	m, fb, _, _, _, _ := newMutator(t)

	// 330621952 is the owner (first AllowFrom) — removing it would lock the
	// operator out, so it must be refused before rendering a confirm.
	_, rpcErr := m.Mutate(context.Background(), "remove_allow", args(t, map[string]string{"chat_id": "330621952"}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeMutateRejected, rpcErr.Code)
	assert.Empty(t, fb.mutationConfirms)
}

func TestMutateNoOwnerTargetRejectsTier3(t *testing.T) {
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	// Empty AllowFrom → no owner to confirm to.
	require.NoError(t, store.Save(access.State{DMPolicy: access.PolicyAllowlist, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{}}))

	router := NewRouter()
	sh := regShim(router, "bbbbbbbbbbbb", "")
	m := NewAdminMutator(AdminMutateConfig{
		Store: store, Router: router, Bot: &fakeBot{},
		Pending: NewPendingStore(dir), Audit: NewAdminAudit(dir, 0),
	})

	_, rpcErr := m.Mutate(context.Background(), "evict_session", args(t, map[string]string{"target": sh.Alias}))
	require.NotNil(t, rpcErr)
	assert.Equal(t, ipc.CodeMutateRejected, rpcErr.Code)
}

// --- Tier-3 apply dispatch (exercised directly until Resolve wires the tap) ---

func TestApplyEvictNotifiesShutdown(t *testing.T) {
	m, _, router, _, _, _ := newMutator(t)

	var got string

	sh := &Shim{ID: "cafe00010002", Notify: func(method string, _ any) error { got = method; return nil }}
	router.Register(sh)

	res, err := m.applyMutation(context.Background(), "evict_session", args(t, map[string]string{"target": sh.Alias}))
	require.NoError(t, err)
	assert.Equal(t, "notifications/shutdown", got)
	assert.Contains(t, res, "evicted")
}

func TestApplyEvictBlocksAdmin(t *testing.T) {
	m, _, router, _, _, _ := newMutator(t)
	router.Register(&Shim{ID: "9a9a9a9a9a9a", Role: "admin", Notify: func(string, any) error { return nil }})

	_, err := m.applyMutation(context.Background(), "evict_session", args(t, map[string]string{"target": AdminAlias}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin")
}

func TestApplyApprovePairingMovesToAllowlist(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.Pending["c0de01"] = access.Pending{SenderID: "777", ChatID: "777"}
		return true
	}))

	res, err := m.applyMutation(context.Background(), "approve_pairing", args(t, map[string]string{"code": "c0de01"}))
	require.NoError(t, err)
	assert.Contains(t, res, "777")

	st := store.Load()
	assert.Contains(t, st.AllowFrom, "777")
	assert.NotContains(t, st.Pending, "c0de01")
}

func TestApplyApprovePairingUnknownCode(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)
	_, err := m.applyMutation(context.Background(), "approve_pairing", args(t, map[string]string{"code": "nope01"}))
	require.Error(t, err)
}

func TestApplyApprovePairingExpiredCodeRefused(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		// ExpiresAt in the past — RulesCleanup hasn't pruned it yet.
		st.Pending["c0de03"] = access.Pending{SenderID: "111", ChatID: "111", ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()}
		return true
	}))

	_, err := m.applyMutation(context.Background(), "approve_pairing", args(t, map[string]string{"code": "c0de03"}))
	require.Error(t, err, "an expired pairing code must not be approvable")
	assert.NotContains(t, store.Load().AllowFrom, "111")
}

func TestApplyDenyPairingDeletes(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.Pending["c0de02"] = access.Pending{SenderID: "888", ChatID: "888"}
		return true
	}))

	_, err := m.applyMutation(context.Background(), "deny_pairing", args(t, map[string]string{"code": "c0de02"}))
	require.NoError(t, err)
	assert.NotContains(t, store.Load().Pending, "c0de02")
	assert.NotContains(t, store.Load().AllowFrom, "888", "deny must NOT add to allowlist")
}

func TestApplyAddAllow(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)

	_, err := m.applyMutation(context.Background(), "add_allow", args(t, map[string]string{"chat_id": "555"}))
	require.NoError(t, err)
	assert.Contains(t, store.Load().AllowFrom, "555")

	// Idempotent — no duplicate.
	_, err = m.applyMutation(context.Background(), "add_allow", args(t, map[string]string{"chat_id": "555"}))
	require.NoError(t, err)

	count := 0

	for _, c := range store.Load().AllowFrom {
		if c == "555" {
			count++
		}
	}

	assert.Equal(t, 1, count)
}

func TestApplyRemoveAllowBlocksOwner(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)

	_, err := m.applyMutation(context.Background(), "remove_allow", args(t, map[string]string{"chat_id": "330621952"}))
	require.Error(t, err, "owner chat must not be removable")
	assert.Contains(t, store.Load().AllowFrom, "330621952")
}

// Regression: a whitespace/leading-zero variant of the owner chat_id must not
// bypass the lockout guard. add_allow canonicalizes on store; the guard
// compares canonical forms.
func TestApplyRemoveAllowOwnerLockoutCanonicalized(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)

	for _, variant := range []string{" 330621952", "330621952 ", "0330621952"} {
		_, err := m.applyMutation(context.Background(), "remove_allow", args(t, map[string]string{"chat_id": variant}))
		require.Errorf(t, err, "variant %q must be refused", variant)
		assert.Contains(t, store.Load().AllowFrom, "330621952", "owner survives variant %q", variant)
	}
}

func TestApplyAddAllowCanonicalizes(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)

	_, err := m.applyMutation(context.Background(), "add_allow", args(t, map[string]string{"chat_id": " 777 "}))
	require.NoError(t, err)
	assert.Contains(t, store.Load().AllowFrom, "777", "stored canonical, not the padded form")
	assert.NotContains(t, store.Load().AllowFrom, " 777 ")
}

func TestApplyAddAllowRejectsNonNumeric(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)
	_, err := m.applyMutation(context.Background(), "add_allow", args(t, map[string]string{"chat_id": "../etc"}))
	require.Error(t, err)
}

func TestApplyRemoveAllowNonOwner(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.AllowFrom = append(st.AllowFrom, "444")
		return true
	}))

	_, err := m.applyMutation(context.Background(), "remove_allow", args(t, map[string]string{"chat_id": "444"}))
	require.NoError(t, err)
	assert.NotContains(t, store.Load().AllowFrom, "444")
}

func TestApplyAddRule(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)

	_, err := m.applyMutation(context.Background(), "add_rule", args(t, map[string]any{"tool": "Bash", "action": "approve", "path_pattern": "**"}))
	require.NoError(t, err)

	rules := store.Load().Rules
	require.Len(t, rules, 1)
	assert.Equal(t, "Bash", rules[0].Tool)
	assert.Equal(t, access.RuleApprove, rules[0].Action)
}

func TestApplyAddRuleRejectsBadAction(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)
	_, err := m.applyMutation(context.Background(), "add_rule", args(t, map[string]any{"tool": "Bash", "action": "maybe"}))
	require.Error(t, err)
}

func TestApplyRevokeRule(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		access.AddRule(st, access.PermissionRule{ID: "r1", Tool: "Bash", Action: access.RuleApprove})
		return true
	}))

	_, err := m.applyMutation(context.Background(), "revoke_rule", args(t, map[string]string{"id": "r1"}))
	require.NoError(t, err)
	assert.Empty(t, store.Load().Rules)
}

func TestApplyRevokeRuleNotFound(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)
	_, err := m.applyMutation(context.Background(), "revoke_rule", args(t, map[string]string{"id": "ghost"}))
	require.Error(t, err)
}

func TestApplyBroadcastSendsToAllowlist(t *testing.T) {
	m, fb, _, store, _, _ := newMutator(t)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.AllowFrom = append(st.AllowFrom, "999")
		return true
	}))

	res, err := m.applyMutation(context.Background(), "broadcast_message", args(t, map[string]string{"text": "heads up"}))
	require.NoError(t, err)
	assert.Contains(t, res, "delivered")

	// Must reach EVERY allowlisted chat — a send-count bug that only messaged the
	// last chat would slip past an only-last-send assertion.
	require.Len(t, fb.sentCalls, 2)
	assert.ElementsMatch(t, []string{"330621952", "999"},
		[]string{fb.sentCalls[0].chatID, fb.sentCalls[1].chatID})

	for _, c := range fb.sentCalls {
		assert.Equal(t, "heads up", c.text)
	}
}

func TestApplyBroadcastAllFailReturnsError(t *testing.T) {
	m, fb, _, _, _, _ := newMutator(t)
	fb.sentMessage.retErr = errors.New("telegram down")

	_, err := m.applyMutation(context.Background(), "broadcast_message", args(t, map[string]string{"text": "x"}))
	require.Error(t, err, "a broadcast that reached nobody must be an error, not a 'delivered' success")
}

func TestAddRuleReturnsRuleID(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)

	res, err := m.applyMutation(context.Background(), "add_rule", args(t, map[string]any{"tool": "Bash", "action": "deny"}))
	require.NoError(t, err)
	assert.Contains(t, res, "id=", "add_rule result must include the assigned rule id for a one-shot revoke")
}

// TestEvictBindsShimIDImmuneToAliasReuse: a Tier-3 evict proposal freezes the
// resolved shim_id, so if the alias is reused by a new session before the owner
// taps, apply targets the original (now-gone) shim — never the reuse.
func TestEvictBindsShimIDImmuneToAliasReuse(t *testing.T) {
	m, _, router, _, _, _ := newMutator(t)

	var evicted []string

	rec := func(tag string) func(string, any) error {
		return func(method string, _ any) error {
			if method == ipc.NotifyShutdown {
				evicted = append(evicted, tag)
			}

			return nil
		}
	}

	orig := &Shim{ID: "1111111111aa", Notify: rec("orig")}
	router.Register(orig)

	res, rpcErr := m.Mutate(context.Background(), "evict_session", args(t, map[string]string{"target": orig.Alias}))
	require.Nil(t, rpcErr)
	require.True(t, res.Pending)

	// orig disconnects; a new shim reuses the freed alias.
	router.Drop(orig.ID)
	reuse := &Shim{ID: "2222222222bb", Notify: rec("reuse")}
	router.Register(reuse)
	require.Equal(t, orig.Alias, reuse.Alias, "precondition: alias reused")

	applied, _ := m.Resolve(context.Background(), res.PendingID, true)
	assert.False(t, applied, "frozen shim_id is gone → no eviction")
	assert.NotContains(t, evicted, "reuse", "must never evict the alias-reuse session")
}

// TestApprovePairingBindsSenderRejectsSwap: a Tier-3 approve_pairing proposal
// freezes the sender it showed the owner; if the code is reused for a different
// sender before the tap, apply refuses rather than allowlisting the new sender.
func TestApprovePairingBindsSenderRejectsSwap(t *testing.T) {
	m, _, _, store, _, _ := newMutator(t)
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.Pending = map[string]access.Pending{"code1": {SenderID: "111", ChatID: "111"}}
		return true
	}))

	res, rpcErr := m.Mutate(context.Background(), "approve_pairing", args(t, map[string]string{"code": "code1"}))
	require.Nil(t, rpcErr)
	require.True(t, res.Pending)

	// Code reused for a different sender before the owner taps.
	require.NoError(t, store.Mutate(func(st *access.State) bool {
		st.Pending["code1"] = access.Pending{SenderID: "999", ChatID: "999"}
		return true
	}))

	applied, _ := m.Resolve(context.Background(), res.PendingID, true)
	assert.False(t, applied, "sender swapped under the code → must not approve")
	assert.NotContains(t, store.Load().AllowFrom, "999", "the swapped-in sender must NOT be allowlisted")
}

// --- Resolve (owner tap) ---

func TestResolveConfirmApplies(t *testing.T) {
	m, _, router, _, _, _ := newMutator(t)

	var notified string

	sh := &Shim{ID: "cafef00d0001", Notify: func(method string, _ any) error { notified = method; return nil }}
	router.Register(sh)

	require.NoError(t, m.pending.Put(PendingMutation{
		ID: "11aa22bb", Tool: "evict_session",
		Args: args(t, map[string]string{"target": sh.Alias}), Summary: "evict " + sh.Alias,
	}))

	applied, detail := m.Resolve(context.Background(), "11aa22bb", true)
	assert.True(t, applied)
	assert.Contains(t, detail, "evicted")
	assert.Equal(t, "notifications/shutdown", notified)

	// Pending is consumed — a second tap finds nothing.
	applied2, _ := m.Resolve(context.Background(), "11aa22bb", true)
	assert.False(t, applied2)
}

func TestResolveCancelDoesNotApply(t *testing.T) {
	m, _, router, _, _, _ := newMutator(t)

	notified := false
	sh := &Shim{ID: "cafef00d0002", Notify: func(string, any) error { notified = true; return nil }}
	router.Register(sh)

	require.NoError(t, m.pending.Put(PendingMutation{
		ID: "33cc44dd", Tool: "evict_session",
		Args: args(t, map[string]string{"target": sh.Alias}), Summary: "evict " + sh.Alias,
	}))

	applied, detail := m.Resolve(context.Background(), "33cc44dd", false)
	assert.False(t, applied)
	assert.Contains(t, detail, "cancel")
	assert.False(t, notified, "cancel must not apply the mutation")
}

func TestResolveUnknownPendingID(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)

	applied, detail := m.Resolve(context.Background(), "deadbeef", true)
	assert.False(t, applied)
	assert.NotEmpty(t, detail)
}

func TestResolveConfirmApplyFailureSurfaces(t *testing.T) {
	m, _, _, _, _, _ := newMutator(t)

	// Target was never registered → apply (evict) fails at resolve time.
	require.NoError(t, m.pending.Put(PendingMutation{
		ID: "55ee66ff", Tool: "evict_session",
		Args: args(t, map[string]string{"target": "s9"}), Summary: "evict s9",
	}))

	applied, detail := m.Resolve(context.Background(), "55ee66ff", true)
	assert.False(t, applied)
	assert.Contains(t, detail, "fail")
}

func TestNotifierResolveMutationDelegates(t *testing.T) {
	m, _, router, store, _, _ := newMutator(t)

	var notified string

	sh := &Shim{ID: "abcdef012345", Notify: func(method string, _ any) error { notified = method; return nil }}
	router.Register(sh)

	require.NoError(t, m.pending.Put(PendingMutation{
		ID: "abcd1234", Tool: "evict_session",
		Args: args(t, map[string]string{"target": sh.Alias}), Summary: "evict",
	}))

	n := NewNotifier(router, store, nil)
	n.SetMutator(m)

	applied, _ := n.ResolveMutation(context.Background(), "abcd1234", true)
	assert.True(t, applied)
	assert.Equal(t, "notifications/shutdown", notified)
}

func TestNotifierResolveMutationNilMutator(t *testing.T) {
	n := NewNotifier(NewRouter(), nil, nil)

	applied, detail := n.ResolveMutation(context.Background(), "abcd1234", true)
	assert.False(t, applied)
	assert.Contains(t, detail, "not enabled")
}

func TestSweepExpiredAuditsAndEditsMessage(t *testing.T) {
	m, fb, _, _, _, _ := newMutator(t)

	require.NoError(t, m.pending.Put(PendingMutation{
		ID: "ab12cd", Tool: "evict_session", Summary: "evict @s2",
		ConfirmChatID: 330621952, ConfirmMessageID: 99,
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}))

	m.sweepExpired(context.Background())

	assert.Equal(t, 99, fb.editedMessage.messageID)
	assert.Contains(t, fb.editedMessage.text, "xpired")

	_, ok := m.pending.Take("ab12cd")
	assert.False(t, ok, "expired pending is consumed by the sweep")
}
