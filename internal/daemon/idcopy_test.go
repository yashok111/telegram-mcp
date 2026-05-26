package daemon

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/yakov/telegram-mcp/internal/bot"
)

// These builders render daemon-side system messages as MarkdownV2 with ids
// wrapped in tap-to-copy inline-code spans. The invariants below are what the
// caller relies on: the id is code-wrapped, and every dynamic fragment that can
// carry MarkdownV2 specials (workdir, error text, claude output) is escaped so
// the Bot API never 400s on the send.

func TestBgMessages_idIsTapToCopy(t *testing.T) {
	const id = "a1b2c3"

	assert.Contains(t, bgStartedMsg(id, "/x", "hi"), bot.MdCode(id))
	assert.Contains(t, bgProgressMsg(id, 1, 2, "out"), bot.MdCode(id))
	assert.Contains(t, bgDoneMsg(id, time.Second, 0.01, 3), bot.MdCode(id))
	assert.Contains(t, bgFailedMsg(id, errors.New("boom")), bot.MdCode(id))
	assert.Contains(t, bgStartFailedMsg(id, errors.New("boom")), bot.MdCode(id))
	assert.Contains(t, bgCancelledMsg(id, time.Second), bot.MdCode(id))
}

func TestSpawnMessages_idAndPidAreTapToCopy(t *testing.T) {
	const id = "deadbeef"

	started := spawnStartedMsg(id, 4242, "/home/u/p")
	assert.Contains(t, started, bot.MdCode(id))
	assert.Contains(t, started, bot.MdCode(strconv.Itoa(4242)))
	assert.Contains(t, started, "🚀 Spawn")

	failed := spawnStartFailedMsg(id, errors.New("nope"))
	assert.Contains(t, failed, bot.MdCode(id))
	assert.Contains(t, failed, "❌ Spawn")
}

// Dynamic fragments holding MarkdownV2 specials must be escaped — a workdir with
// a '.' or an error with '()' would otherwise break the parser. The escaped form
// (backslash-prefixed) must appear; the bare special must not appear outside the
// id's code span.
func TestBuilders_escapeSpecialsInDynamicText(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string // an escaped fragment that must be present
	}{
		{"bg started workdir dot", bgStartedMsg("id", "/a.b/c-d", "p"), `/a\.b/c\-d`},
		{"bg failed error parens", bgFailedMsg("id", errors.New("oops (x)")), `oops \(x\)`},
		{"bg done cost dot", bgDoneMsg("id", time.Second, 0.5, 1), `$0\.5000`},
		{"spawn started workdir dot", spawnStartedMsg("id", 1, "/a.b"), `/a\.b`},
		{"spawn failed error dot", spawnStartFailedMsg("id", errors.New("exec /a.b/claude failed")), `/a\.b/claude`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, tt.got, tt.want)
		})
	}
}

// The whole message must remain MarkdownV2-parseable: outside the id's code
// span every literal special is escaped. Spot-check that the prompt label's
// colon-free static text and the closing sentence period are escaped.
func TestSpawnStartedMsg_staticPunctuationEscaped(t *testing.T) {
	msg := spawnStartedMsg("id", 1, "/x")
	// trailing sentence ends in an escaped period, never a bare one
	assert.True(t, strings.HasSuffix(msg, `to talk to it\.`), "trailing period escaped: %q", msg)
}
