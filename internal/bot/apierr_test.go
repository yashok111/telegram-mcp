package bot

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsPermanentChatError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"thread deleted", errors.New("telego: editMessageText: api: 400 \"Bad Request: message thread not found\""), true},
		{"chat not found", errors.New("telego: sendMessage: api: 400 \"Bad Request: chat not found\""), true},
		{"bot kicked", errors.New("telego: sendMessage: api: 403 \"Forbidden: bot was kicked from the supergroup chat\""), true},
		{"bot blocked", errors.New("telego: sendMessage: api: 403 \"Forbidden: bot was blocked by the user\""), true},
		{"user deactivated", errors.New("telego: sendMessage: api: 403 \"Forbidden: user is deactivated\""), true},
		{"not a member", errors.New("telego: sendMessage: api: 403 \"Forbidden: bot is not a member of the channel chat\""), true},
		{"peer id invalid", errors.New("telego: sendMessage: api: 400 \"Bad Request: PEER_ID_INVALID\""), true},
		{"peer id invalid lowercase", errors.New("api: peer_id_invalid"), true},
		{"topic id invalid", errors.New("telego: closeForumTopic: api: 400 \"Bad Request: TOPIC_ID_INVALID\""), true},
		// Transient / recoverable — must NOT be classified permanent.
		{"flood wait", errors.New("telego: sendMessage: api: 429 \"Too Many Requests: retry after 5\""), false},
		{"not enough rights", errors.New("telego: sendMessage: api: 400 \"Bad Request: not enough rights to send text messages\""), false},
		{"message to edit not found", errors.New("telego: editMessageText: api: 400 \"Bad Request: message to edit not found\""), false},
		{"network", errors.New("dial tcp: i/o timeout"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsPermanentChatError(tc.err))
		})
	}
}
