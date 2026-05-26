package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yakov/telegram-mcp/internal/access"
)

func TestTopicArgs(t *testing.T) {
	assert.Empty(t, topicArgs("/topic"))
	assert.Empty(t, topicArgs("  /topic  "))
	assert.Equal(t, []string{"close"}, topicArgs("/topic close"))
	assert.Equal(t, []string{"close", "extra"}, topicArgs("/topic close extra"))
	assert.Equal(t, []string{"close"}, topicArgs("/topic   close"), "multiple spaces collapsed")
}

type fakeTopicCloser struct {
	called    []int
	returnErr error
}

func (f *fakeTopicCloser) CloseTopic(_ context.Context, threadID int) error {
	f.called = append(f.called, threadID)
	return f.returnErr
}

func TestHandleTopicClose_callsTopicCloser(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	fc := &fakeTopicCloser{}
	b.SetTopicCloser(fc)

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic close",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	assert.Equal(t, []int{9}, fc.called, "TopicCloser invoked with correct thread_id")
	assert.Empty(t, api.recordedCalls("sendMessage"), "no error reply on success")
}

func TestHandleTopicClose_surfacesErrorInTopic(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy: access.PolicyAllowlist, AllowFrom: []string{"42"},
		Groups: map[string]access.GroupPolicy{}, Pending: map[string]access.Pending{},
		ForumChatID: -100777,
	})

	fc := &fakeTopicCloser{returnErr: errors.New("boom")}
	b.SetTopicCloser(fc)

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic close",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Topic close failed")
}

func TestTopicGate_forumDisabled_replies(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: 0, // disabled
	})

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic close",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Forum routing is disabled")
}

func TestTopicGate_wrongChat_replies(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100999, Type: "supergroup"}, // wrong chat
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic close",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "only runs inside the configured forum supergroup")
}

func TestTopicGate_inGeneralThread_replies(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 0, // General thread
		Text:            "/topic close",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Use this command inside a topic")
}

func TestTopicGate_nonAllowlistedSender_replies(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"}, // user 99 NOT in list
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 99},
		MessageThreadID: 9,
		Text:            "/topic close",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Not authorized")
}

func TestSendPlain_appliesThreadID(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})

	b.sendPlain(t.Context(), 42, 9, "hi")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.EqualValues(t, 9, calls[0].params["message_thread_id"], "topic reply lands in originating thread")
}

func TestSendPlain_zeroThreadID_omitsField(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{})

	b.sendPlain(t.Context(), 42, 0, "hi")

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	_, present := calls[0].params["message_thread_id"]
	assert.False(t, present)
}

type stubRouterView struct {
	snap []ShimInfo
}

func (s *stubRouterView) Snapshot() []ShimInfo { return s.snap }
func (s *stubRouterView) Pin(string, string, time.Duration) (ShimInfo, error) {
	return ShimInfo{}, nil
}
func (s *stubRouterView) Unpin(string) bool                         { return false }
func (s *stubRouterView) Evict(string) (ShimInfo, error)            { return ShimInfo{}, nil }
func (s *stubRouterView) SetLabel(string, string) (ShimInfo, error) { return ShimInfo{}, nil }

func TestHandleTopicInfo_rendersOwnerDetails(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	b.router = &stubRouterView{snap: []ShimInfo{
		{ID: "shim-a", Alias: "s1", Label: "foo", Workdir: "/projects/foo", TopicID: 9, ConnectedAt: time.Now().UTC()},
	}}

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "@s1")
	assert.Contains(t, text, "foo")
	assert.Contains(t, text, "/projects/foo")
}

func TestHandleTopicInfo_orphanTopic_replies(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})
	b.router = &stubRouterView{}

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "no attached shim")
}

func TestHandleTopicRename_callsEditForumTopic(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic rename new name",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("editForumTopic")
	require.Len(t, calls, 1)
	assert.Equal(t, "new name", calls[0].params["name"])
	assert.EqualValues(t, 9, calls[0].params["message_thread_id"])
}

func TestHandleTopicRename_tooLong_rejectsBeforeAPI(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	longName := strings.Repeat("x", 129)
	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic rename " + longName,
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	assert.Empty(t, api.recordedCalls("editForumTopic"),
		"pre-flight rejection must skip the API call")
	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "too long")
}

func TestHandleTopicRename_missingName_showsUsage(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic rename",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Usage: /topic rename")
}

func TestHandleTopicsList_DMRendersAllTopics(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
		TopicsByThread: map[string]access.TopicMeta{
			"7":  {ThreadID: 7, Label: "foo", Workdir: "/p/foo", LockedBy: "shim-a"},
			"9":  {ThreadID: 9, Label: "bar", Workdir: "/p/bar"},
			"11": {ThreadID: 11, Workdir: "/p/baz"},
		},
	})

	msg := telego.Message{
		Chat: telego.Chat{ID: 42, Type: "private"},
		From: &telego.User{ID: 42},
		Text: "/topics list",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	text, _ := calls[0].params["text"].(string)
	assert.Contains(t, text, "Topics (3 total")
	assert.Contains(t, text, "7 · foo")
	assert.Contains(t, text, "locked by shim-a")
	assert.Contains(t, text, "9 · bar")
}

func TestHandleTopicsList_inGroup_silent(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	msg := telego.Message{
		Chat: telego.Chat{ID: -100777, Type: "supergroup"},
		From: &telego.User{ID: 42},
		Text: "/topics list",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	assert.Empty(t, api.recordedCalls("sendMessage"), "DM-only command silent in group")
}

func TestHandleTopicsList_nonAllowlisted_silent(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})

	msg := telego.Message{
		Chat: telego.Chat{ID: 99, Type: "private"},
		From: &telego.User{ID: 99}, // not in AllowFrom
		Text: "/topics list",
	}
	// onCommand only routes commands from allowlisted senders, so we expect
	// no API calls at all.
	require.NoError(t, b.handleCommand(t.Context(), msg))
	assert.Empty(t, api.recordedCalls("sendMessage"))
}

func TestHandleTopicClose_unknownSubcommand_showsHelp(t *testing.T) {
	b, api, _ := newTestBot(t, access.State{
		DMPolicy:    access.PolicyAllowlist,
		AllowFrom:   []string{"42"},
		Groups:      map[string]access.GroupPolicy{},
		Pending:     map[string]access.Pending{},
		ForumChatID: -100777,
	})
	b.SetTopicCloser(&fakeTopicCloser{})

	msg := telego.Message{
		Chat:            telego.Chat{ID: -100777, Type: "supergroup"},
		From:            &telego.User{ID: 42},
		MessageThreadID: 9,
		Text:            "/topic unknown",
	}
	require.NoError(t, b.handleCommand(t.Context(), msg))

	calls := api.recordedCalls("sendMessage")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].params["text"], "Usage:")
}
