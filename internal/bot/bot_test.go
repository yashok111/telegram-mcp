package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestMain(m *testing.M) {
	// fasthttp's background sweepers (connsCleaner, mCleaner, etc.) outlive
	// the HostClient/Client they belong to until GC, which is fine in
	// production but trips goleak in tests. telego inherits them via its
	// default Caller.
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*HostClient).connsCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*Client).mCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*TCPDialer).tcpAddrsClean"),
		// telego's long-polling goroutine sleeps in a backoff loop after
		// ctx cancellation; it exits eventually but not within goleak's
		// inspection window.
		goleak.IgnoreAnyFunction("github.com/mymmrac/telego.(*Bot).doLongPolling"),
	)
}

// ===== pure helpers =====

func TestCommandName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/start", "start"},
		{"/start@my_bot", "start"},
		{"/start hello", "start"},
		{"/help@bot args", "help"},
		{"no slash", ""},
		{"/", ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, commandName(tt.in), "commandName(%q)", tt.in)
	}
}

func TestSafeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"plain.txt", "plain.txt"},
		{"with<tag>", "with_tag_"},
		{"line\nbreak", "line_break"},
		{"a[b]c;d\rE", "a_b_c_d_E"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, safeName(tt.in), "safeName(%q)", tt.in)
	}
}

func TestSafeExt(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"file.PNG", "PNG"},
		{"path/to/foo.jpg", "jpg"},
		{"no_ext", ""},
		{"weird.jp!eg", "jpeg"}, // non-alnum stripped
		{".hidden", "hidden"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, safeExt(tt.in), "safeExt(%q)", tt.in)
	}
}

func TestSanitizeID(t *testing.T) {
	assert.Equal(t, "abc-123_x", sanitizeID("abc-123_x"))
	assert.Equal(t, "abc123", sanitizeID("abc 123!@#"))
	assert.Empty(t, sanitizeID("$$$"))
}

func TestUserLabel(t *testing.T) {
	assert.Empty(t, userLabel(nil))
	assert.Equal(t, "@bob", userLabel(&telego.User{Username: "bob", ID: 5}))
	assert.Equal(t, "42", userLabel(&telego.User{ID: 42}))
}

func TestParseChatID(t *testing.T) {
	id, err := parseChatID("-100123")
	require.NoError(t, err)
	assert.Equal(t, int64(-100123), id)

	_, err = parseChatID("not-a-number")
	assert.Error(t, err)
}

func TestFindPendingFor(t *testing.T) {
	pending := map[string]access.Pending{
		"aaaaaa": {SenderID: "111"},
		"bbbbbb": {SenderID: "222"},
	}
	code, p, ok := findPendingFor(pending, "222")
	assert.True(t, ok)
	assert.Equal(t, "bbbbbb", code)
	assert.Equal(t, "222", p.SenderID)

	_, _, ok = findPendingFor(pending, "999")
	assert.False(t, ok)
}

// ===== attachmentMeta =====

func TestAttachmentMeta_nil_returnsNil(t *testing.T) {
	// Empty message — no media.
	msg := &telego.Message{}
	assert.Nil(t, attachmentMeta(msg))
}

func TestAttachmentMeta_document(t *testing.T) {
	msg := &telego.Message{Document: &telego.Document{
		FileID: "doc1", FileSize: 1024, MimeType: "application/pdf", FileName: "report.pdf",
	}}
	m := attachmentMeta(msg)
	assert.Equal(t, "document", m["attachment_kind"])
	assert.Equal(t, "doc1", m["attachment_file_id"])
	assert.Equal(t, "1024", m["attachment_size"])
	assert.Equal(t, "application/pdf", m["attachment_mime"])
	assert.Equal(t, "report.pdf", m["attachment_name"])
}

func TestAttachmentMeta_voice_audio_video_videoNote_sticker(t *testing.T) {
	tests := []struct {
		name string
		msg  *telego.Message
		kind string
	}{
		{"voice", &telego.Message{Voice: &telego.Voice{FileID: "v1", FileSize: 100, MimeType: "audio/ogg"}}, "voice"},
		{"audio", &telego.Message{Audio: &telego.Audio{FileID: "a1", FileSize: 200, FileName: "song.mp3"}}, "audio"},
		{"video", &telego.Message{Video: &telego.Video{FileID: "vi1", FileSize: 500}}, "video"},
		{"video_note", &telego.Message{VideoNote: &telego.VideoNote{FileID: "vn1", FileSize: 10}}, "video_note"},
		{"sticker", &telego.Message{Sticker: &telego.Sticker{FileID: "s1", FileSize: 50}}, "sticker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := attachmentMeta(tt.msg)
			require.NotNil(t, m)
			assert.Equal(t, tt.kind, m["attachment_kind"])
			assert.NotEmpty(t, m["attachment_file_id"])
		})
	}
}

// ===== messageContent =====

func TestMessageContent(t *testing.T) {
	tests := []struct {
		name string
		msg  telego.Message
		want string
	}{
		{"text wins", telego.Message{Text: "hi"}, "hi"},
		{"caption falls through", telego.Message{Caption: "cap"}, "cap"},
		{"photo no caption", telego.Message{Photo: []telego.PhotoSize{{}}}, "(photo)"},
		{"document with name", telego.Message{Document: &telego.Document{FileName: "x.pdf"}}, "(document: x.pdf)"},
		{"document unnamed", telego.Message{Document: &telego.Document{}}, "(document: file)"},
		{"voice", telego.Message{Voice: &telego.Voice{}}, "(voice message)"},
		{"audio", telego.Message{Audio: &telego.Audio{}}, "(audio)"},
		{"video", telego.Message{Video: &telego.Video{}}, "(video)"},
		{"video_note", telego.Message{VideoNote: &telego.VideoNote{}}, "(video note)"},
		{"sticker no emoji", telego.Message{Sticker: &telego.Sticker{}}, "(sticker)"},
		{"sticker with emoji", telego.Message{Sticker: &telego.Sticker{Emoji: "🐶"}}, "(sticker 🐶)"},
		{"empty", telego.Message{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, messageContent(tt.msg))
		})
	}
}

// ===== isMentioned =====

func TestIsMentioned_byEntity(t *testing.T) {
	b := &Bot{username: "my_bot"}
	msg := &telego.Message{
		Text:     "hey @my_bot what's up",
		Entities: []telego.MessageEntity{{Type: "mention", Offset: 4, Length: 7}},
	}
	assert.True(t, b.isMentioned(msg, nil))
}

func TestIsMentioned_textMentionEntity(t *testing.T) {
	b := &Bot{username: "my_bot"}
	msg := &telego.Message{
		Text:     "x",
		Entities: []telego.MessageEntity{{Type: "text_mention", Offset: 0, Length: 1, User: &telego.User{IsBot: true, Username: "my_bot"}}},
	}
	assert.True(t, b.isMentioned(msg, nil))
}

func TestIsMentioned_replyToOurMessageCounts(t *testing.T) {
	b := &Bot{username: "my_bot"}
	msg := &telego.Message{
		Text:           "ok",
		ReplyToMessage: &telego.Message{From: &telego.User{Username: "my_bot"}},
	}
	assert.True(t, b.isMentioned(msg, nil))
}

func TestIsMentioned_extraPattern(t *testing.T) {
	b := &Bot{username: "my_bot"}
	msg := &telego.Message{Text: "Hey claude, are you there?"}
	assert.True(t, b.isMentioned(msg, []string{`\bclaude\b`}))
}

func TestIsMentioned_invalidExtraPatternSkipped(t *testing.T) {
	b := &Bot{username: "my_bot"}
	msg := &telego.Message{Text: "ping"}
	// Invalid regex should be silently skipped, not panic.
	assert.False(t, b.isMentioned(msg, []string{`[unclosed`}))
}

func TestIsMentioned_noMatch(t *testing.T) {
	b := &Bot{username: "my_bot"}
	msg := &telego.Message{Text: "random chat"}
	assert.False(t, b.isMentioned(msg, nil))
}

func TestIsMentioned_captionEntitiesUsedWhenTextEmpty(t *testing.T) {
	b := &Bot{username: "my_bot"}
	msg := &telego.Message{
		Caption:         "see @my_bot",
		CaptionEntities: []telego.MessageEntity{{Type: "mention", Offset: 4, Length: 7}},
	}
	assert.True(t, b.isMentioned(msg, nil))
}

// ===== gate =====

func newGateBot(t *testing.T, st access.State) (*Bot, *access.Store) {
	t.Helper()
	dir := t.TempDir()
	store := access.NewStore(dir, false)
	require.NoError(t, store.Save(st))

	return &Bot{store: store, username: "my_bot"}, store
}

func TestGate_disabledPolicy_drops(t *testing.T) {
	b, _ := newGateBot(t, access.State{DMPolicy: access.PolicyDisabled, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{}})
	res := b.gate(&telego.Message{From: &telego.User{ID: 1}, Chat: telego.Chat{Type: "private", ID: 1}})
	assert.Equal(t, actionDrop, res.action)
}

func TestGate_dm_allowlisted_delivers(t *testing.T) {
	b, _ := newGateBot(t, access.State{DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{}})
	res := b.gate(&telego.Message{From: &telego.User{ID: 42}, Chat: telego.Chat{Type: "private", ID: 42}})
	assert.Equal(t, actionDeliver, res.action)
}

func TestGate_dm_allowlist_dropsUnknown(t *testing.T) {
	b, _ := newGateBot(t, access.State{DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{}})
	res := b.gate(&telego.Message{From: &telego.User{ID: 999}, Chat: telego.Chat{Type: "private", ID: 999}})
	assert.Equal(t, actionDrop, res.action)
}

func TestGate_dm_pairing_newCode(t *testing.T) {
	b, _ := newGateBot(t, access.State{DMPolicy: access.PolicyPairing, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{}})
	res := b.gate(&telego.Message{From: &telego.User{ID: 7}, Chat: telego.Chat{Type: "private", ID: 7}})
	assert.Equal(t, actionPair, res.action)
	assert.False(t, res.isResend)
	assert.Len(t, res.code, 6)
}

func TestGate_dm_pairing_resendCode(t *testing.T) {
	now := time.Now().UnixMilli()
	b, _ := newGateBot(t, access.State{
		DMPolicy:  access.PolicyPairing,
		AllowFrom: []string{},
		Groups:    map[string]access.GroupPolicy{},
		Pending: map[string]access.Pending{
			"abcdef": {SenderID: "7", ChatID: "7", CreatedAt: now, ExpiresAt: now + 60_000, Replies: 1},
		},
	})
	res := b.gate(&telego.Message{From: &telego.User{ID: 7}, Chat: telego.Chat{Type: "private", ID: 7}})
	assert.Equal(t, actionPair, res.action)
	assert.True(t, res.isResend)
	assert.Equal(t, "abcdef", res.code)
}

func TestGate_dm_pairing_dropsAfterTwoReplies(t *testing.T) {
	now := time.Now().UnixMilli()
	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyPairing, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{},
		Pending: map[string]access.Pending{
			"abcdef": {SenderID: "7", ExpiresAt: now + 60_000, Replies: 2},
		},
	})
	res := b.gate(&telego.Message{From: &telego.User{ID: 7}, Chat: telego.Chat{Type: "private", ID: 7}})
	assert.Equal(t, actionDrop, res.action)
}

func TestGate_dm_pairing_dropsWhenPendingFull(t *testing.T) {
	now := time.Now().UnixMilli()

	pending := map[string]access.Pending{}
	for i := range 3 {
		pending[string(rune('a'+i))+"bcdef"] = access.Pending{SenderID: "100", ExpiresAt: now + 60_000, Replies: 1}
	}

	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyPairing, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: pending,
	})
	// New sender — pending already at cap.
	res := b.gate(&telego.Message{From: &telego.User{ID: 500}, Chat: telego.Chat{Type: "private", ID: 500}})
	assert.Equal(t, actionDrop, res.action)
}

func TestGate_group_unknown_drops(t *testing.T) {
	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	res := b.gate(&telego.Message{From: &telego.User{ID: 1}, Chat: telego.Chat{Type: "group", ID: -100}})
	assert.Equal(t, actionDrop, res.action)
}

func TestGate_group_allowFrom_filtered(t *testing.T) {
	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{},
		Groups: map[string]access.GroupPolicy{
			"-100": {RequireMention: false, AllowFrom: []string{"42"}},
		},
		Pending: map[string]access.Pending{},
	})
	// Sender not in group AllowFrom.
	res := b.gate(&telego.Message{From: &telego.User{ID: 999}, Chat: telego.Chat{Type: "group", ID: -100}})
	assert.Equal(t, actionDrop, res.action)
	// Sender is in group AllowFrom.
	res = b.gate(&telego.Message{From: &telego.User{ID: 42}, Chat: telego.Chat{Type: "group", ID: -100}})
	assert.Equal(t, actionDeliver, res.action)
}

func TestGate_group_requireMention_blocksUntilMentioned(t *testing.T) {
	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyAllowlist,
		Groups: map[string]access.GroupPolicy{
			"-100": {RequireMention: true},
		},
		Pending: map[string]access.Pending{},
	})
	// Plain text — no mention → drop.
	res := b.gate(&telego.Message{From: &telego.User{ID: 1}, Chat: telego.Chat{Type: "group", ID: -100}, Text: "hi"})
	assert.Equal(t, actionDrop, res.action)
	// Mention entity present → deliver.
	res = b.gate(&telego.Message{
		From: &telego.User{ID: 1}, Chat: telego.Chat{Type: "group", ID: -100},
		Text:     "hey @my_bot",
		Entities: []telego.MessageEntity{{Type: "mention", Offset: 4, Length: 7}},
	})
	assert.Equal(t, actionDeliver, res.action)
}

func TestGate_supergroup_treatedLikeGroup(t *testing.T) {
	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyAllowlist,
		Groups: map[string]access.GroupPolicy{
			"-1001": {RequireMention: false},
		},
		Pending: map[string]access.Pending{},
	})
	res := b.gate(&telego.Message{From: &telego.User{ID: 1}, Chat: telego.Chat{Type: "supergroup", ID: -1001}})
	assert.Equal(t, actionDeliver, res.action)
}

func TestGate_nilFrom_drops(t *testing.T) {
	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	res := b.gate(&telego.Message{Chat: telego.Chat{Type: "private", ID: 1}})
	assert.Equal(t, actionDrop, res.action)
}

func TestGate_unknownChatType_drops(t *testing.T) {
	b, _ := newGateBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{}, Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	res := b.gate(&telego.Message{From: &telego.User{ID: 1}, Chat: telego.Chat{Type: "channel", ID: -1}})
	assert.Equal(t, actionDrop, res.action)
}

// permissionReplyRE and callbackRE regex correctness — locked-in invariants.
func TestPermissionReplyRegex(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		yn   string
		code string
	}{
		{"yes abcde", true, "yes", "abcde"},
		{"no abcde", true, "no", "abcde"},
		{"y abcde", true, "y", "abcde"},
		{"n abcde", true, "n", "abcde"},
		{"  YES   ZBCDE  ", true, "YES", "ZBCDE"},
		{"yes abcde extra", false, "", ""},
		{"yes abcdl", false, "", ""}, // 'l' excluded
		{"maybe abcde", false, "", ""},
		{"yes abcd", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			m := permissionReplyRE.FindStringSubmatch(c.in)
			if !c.ok {
				assert.Nil(t, m)
				return
			}

			require.NotNil(t, m)
			assert.Equal(t, c.yn, m[1])
			assert.Equal(t, c.code, m[2])
		})
	}
}

func TestCallbackRegex(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"perm:allow:abcde", true},
		{"perm:deny:abcde", true},
		{"perm:more:abcde", true},
		{"perm:bogus:abcde", false},
		{"perm:allow:abcdl", false}, // 'l' excluded
		{"perm:allow:abcde extra", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			m := callbackRE.FindStringSubmatch(c.in)
			if !c.ok {
				assert.Nil(t, m)
			} else {
				require.NotNil(t, m)
			}
		})
	}
}

func TestCallbackRE_acceptsAtool1h(t *testing.T) {
	assert.True(t, callbackRE.MatchString("perm:atool1h:abcde"))
}

func TestCallbackRE_acceptsAtoolall(t *testing.T) {
	assert.True(t, callbackRE.MatchString("perm:atoolall:abcde"))
}

func TestCallbackRE_acceptsDtoolall(t *testing.T) {
	assert.True(t, callbackRE.MatchString("perm:dtoolall:abcde"))
}

func TestCallbackRE_rejectsBadBehavior(t *testing.T) {
	assert.False(t, callbackRE.MatchString("perm:weird:abcde"))
}

// ===== addRuleAndResolve =====

func newRuleCallbackQuery(id string) *telego.CallbackQuery {
	return &telego.CallbackQuery{
		ID:   "cq-rule",
		From: telego.User{ID: 42},
		Data: "perm:atool1h:" + id,
		Message: &telego.Message{
			MessageID: 1, Chat: telego.Chat{ID: 42, Type: "private"}, Text: "🔐 Permission: Bash",
		},
	}
}

func TestAddRuleAndResolve_atool1h_storesApproveRuleWithExpiry(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	before := time.Now().UnixMilli()
	b.addRuleAndResolve(t.Context(), newRuleCallbackQuery("abcde"), "abcde", access.RuleApprove, time.Hour)

	st := b.store.Load()
	require.Len(t, st.Rules, 1)
	rule := st.Rules[0]
	assert.Equal(t, access.RuleApprove, rule.Action)
	assert.Equal(t, "Bash", rule.Tool)
	assert.Greater(t, rule.ExpiresAt, before)
	assert.Less(t, rule.ExpiresAt, before+2*int64(time.Hour/time.Millisecond))
}

func TestAddRuleAndResolve_atoolall_storesApproveRuleNoExpiry(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	b.addRuleAndResolve(t.Context(), newRuleCallbackQuery("abcde"), "abcde", access.RuleApprove, 0)

	st := b.store.Load()
	require.Len(t, st.Rules, 1)
	assert.Equal(t, access.RuleApprove, st.Rules[0].Action)
	assert.Equal(t, "Bash", st.Rules[0].Tool)
	assert.Equal(t, int64(0), st.Rules[0].ExpiresAt)
}

func TestAddRuleAndResolve_dtoolall_storesDenyRule(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	b.addRuleAndResolve(t.Context(), newRuleCallbackQuery("abcde"), "abcde", access.RuleDeny, 0)

	st := b.store.Load()
	require.Len(t, st.Rules, 1)
	assert.Equal(t, access.RuleDeny, st.Rules[0].Action)
	assert.Equal(t, "Bash", st.Rules[0].Tool)
}

func TestAddRuleAndResolve_unknownToolName_fallsBackToWildcard(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	// noopNotifier returns false for any id != "abcde"
	q := newRuleCallbackQuery("zzzzz")
	q.Data = "perm:atoolall:zzzzz"
	b.addRuleAndResolve(t.Context(), q, "zzzzz", access.RuleApprove, 0)

	st := b.store.Load()
	require.Len(t, st.Rules, 1)
	assert.Equal(t, "*", st.Rules[0].Tool)
}

func TestAddRuleAndResolve_callsResolvePermission(t *testing.T) {
	b, _, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})
	n, _ := b.notifier.(*noopNotifier)

	b.addRuleAndResolve(t.Context(), newRuleCallbackQuery("abcde"), "abcde", access.RuleApprove, time.Hour)
	require.Len(t, n.resolved, 1)
	assert.Equal(t, "abcde", n.resolved[0].requestID)
	assert.Equal(t, "allow", n.resolved[0].behavior)

	b.addRuleAndResolve(t.Context(), newRuleCallbackQuery("abcde"), "abcde", access.RuleDeny, 0)
	require.Len(t, n.resolved, 2)
	assert.Equal(t, "deny", n.resolved[1].behavior)
}

func TestCallback_atool1h_notAllowlisted_doesNotAddRule(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	q := telego.CallbackQuery{
		ID:   "cq-stranger",
		From: telego.User{ID: 999},
		Data: "perm:atool1h:abcde",
		Message: &telego.Message{
			MessageID: 1, Chat: telego.Chat{ID: 999, Type: "private"}, Text: "🔐 Permission: Bash",
		},
	}
	require.NoError(t, b.handleCallback(t.Context(), q))

	st := b.store.Load()
	assert.Empty(t, st.Rules, "non-allowlisted callback must not add a rule")

	calls := api.recordedCalls("answerCallbackQuery")
	require.NotEmpty(t, calls)
	assert.Contains(t, payloadString(calls[0].params), "Not authorized")

	n, _ := b.notifier.(*noopNotifier)
	assert.Empty(t, n.resolved, "non-allowlisted callback must not resolve permission")
}

func TestBroadcastPermissionRequest_keyboardIncludesNewRows(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
	})

	b.BroadcastPermissionRequest(t.Context(), "abcde", "Bash")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	body := payloadString(calls[0].params)
	assert.Contains(t, body, "perm:allow:abcde")
	assert.Contains(t, body, "perm:deny:abcde")
	assert.Contains(t, body, "perm:more:abcde")
	assert.Contains(t, body, "perm:atool1h:abcde")
	assert.Contains(t, body, "perm:atoolall:abcde")
	assert.Contains(t, body, "perm:dtoolall:abcde")

	// Three rows of inline keyboard expected.
	assert.GreaterOrEqual(t, strings.Count(body, "callback_data"), 6)
}
