package bot

import "strings"

// permanentChatErrSubstrings are Telegram Bot API error descriptions meaning the
// destination chat/topic is permanently unreachable for this bot: it was
// removed, the chat/topic was deleted, or the peer is invalid. Retrying such a
// send never succeeds without external action (re-adding the bot, the user
// unblocking), so callers running retry loops must treat these as terminal and
// tear down tracked state instead of re-queuing. Telegram exposes no stable
// numeric codes for these — the description text is the contract. Matched
// case-insensitively against the wrapped telego error.
var permanentChatErrSubstrings = []string{
	"message thread not found",    // forum topic deleted
	"chat not found",              // chat id gone / never existed
	"bot was kicked",              // removed from group/supergroup/channel
	"bot was blocked by the user", // DM recipient blocked the bot
	"user is deactivated",         // recipient account deleted
	"bot is not a member",         // bot not in the target chat
	"peer_id_invalid",             // invalid / stale peer id
	"topic_id_invalid",            // forum topic id gone / never existed
}

// IsPermanentChatError reports whether err is a Telegram failure that will never
// succeed on retry because the chat/topic is gone or the bot can't reach it.
// Retry loops (the topic-header refresh, the closed-topic sweep, the pairing
// confirm ticker) must treat a true result as terminal — purge the tracked
// state, stop re-queuing — to avoid the unbounded error-burst class of bug.
// Transient failures (429 flood-wait, network, recoverable "not enough rights")
// return false and remain retry-worthy.
func IsPermanentChatError(err error) bool {
	if err == nil {
		return false
	}

	low := strings.ToLower(err.Error())
	for _, s := range permanentChatErrSubstrings {
		if strings.Contains(low, s) {
			return true
		}
	}

	return false
}
