package bot

import (
	"context"
	"testing"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
)

// messageContentPredicate gates onMessage: real content passes, while service
// updates (forum topic created/edited, chat joins) and content-less messages
// are dropped before they reach Claude Code as empty inbound — and, post-#71,
// before an empty forum service message can trigger an auto-spawn.
func TestMessageContentPredicate(t *testing.T) {
	pred := messageContentPredicate()

	tests := []struct {
		name   string
		update telego.Update
		want   bool
	}{
		// Service messages carry no user content; Telegram emits them for the
		// daemon's own CreateForumTopic/EditForumTopic calls, which the bot
		// then polls back. They must not reach onMessage.
		{"forum topic created", telego.Update{Message: &telego.Message{ForumTopicCreated: &telego.ForumTopicCreated{}}}, false},
		{"forum topic edited", telego.Update{Message: &telego.Message{ForumTopicEdited: &telego.ForumTopicEdited{}}}, false},
		{"new chat members", telego.Update{Message: &telego.Message{NewChatMembers: []telego.User{{ID: 1}}}}, false},
		{"content-less message", telego.Update{Message: &telego.Message{}}, false},
		{"nil message", telego.Update{}, false},

		// Real content must pass. Location/Contact/Poll/Dice are covered by
		// AnyMessageWithMedia, so this filter must not silently drop them.
		{"text", telego.Update{Message: &telego.Message{Text: "hi"}}, true},
		{"caption only", telego.Update{Message: &telego.Message{Caption: "cap"}}, true},
		{"photo without caption", telego.Update{Message: &telego.Message{Photo: []telego.PhotoSize{{FileID: "f"}}}}, true},
		{"captioned photo", telego.Update{Message: &telego.Message{Photo: []telego.PhotoSize{{FileID: "f"}}, Caption: "cap"}}, true},
		{"location", telego.Update{Message: &telego.Message{Location: &telego.Location{}}}, true},
		{"contact", telego.Update{Message: &telego.Message{Contact: &telego.Contact{}}}, true},
		{"poll", telego.Update{Message: &telego.Message{Poll: &telego.Poll{}}}, true},
		{"dice", telego.Update{Message: &telego.Message{Dice: &telego.Dice{}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pred(context.Background(), tt.update), tt.name)
		})
	}
}
