package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestConsentGate(clock *time.Time) *consentGate {
	g := newConsentGate()
	g.now = func() time.Time { return *clock }

	return g
}

func TestConsentGate_Feed(t *testing.T) {
	splash := "\x1b[1mWARNING: Loading development channels\x1b[0m\r\n  ❯ 1. \x1b[36m" + consentMarker + "\x1b[0m\r\n    2. Exit\r\n"

	tests := []struct {
		name   string
		chunks []string
		want   []bool
	}{
		{
			name:   "no dialog, no enter",
			chunks: []string{"Welcome to Claude Code\r\n", "Do you trust the files in this folder?\r\n ❯ 1. Yes, proceed\r\n"},
			want:   []bool{false, false},
		},
		{
			name:   "consent splash presses once",
			chunks: []string{splash},
			want:   []bool{true},
		},
		{
			name:   "label split across reads",
			chunks: []string{"❯ 1. I am using this for", " local development\r\n"},
			want:   []bool{false, true},
		},
		{
			name:   "same frame is not answered twice",
			chunks: []string{splash, "\x1b[2J", "prompt >"},
			want:   []bool{true, false, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := time.Unix(0, 0)
			g := newTestConsentGate(&clock)
			require.Len(t, tt.want, len(tt.chunks))

			for i, c := range tt.chunks {
				clock = clock.Add(time.Second)
				assert.Equalf(t, tt.want[i], g.feed([]byte(c)), "chunk %d", i)
			}
		})
	}
}

func TestConsentGate_RateLimitAndCap(t *testing.T) {
	clock := time.Unix(0, 0)
	g := newTestConsentGate(&clock)

	assert.True(t, g.feed([]byte(consentMarker)))
	clock = clock.Add(100 * time.Millisecond)
	assert.False(t, g.feed([]byte(consentMarker)), "re-render within the press gap must not double-press")

	presses := 1
	for range 20 {
		clock = clock.Add(time.Second)
		if g.feed([]byte(consentMarker)) {
			presses++
		}
	}

	assert.Equal(t, maxConsentPresses, presses, "a splash that never goes away is not hammered forever")
}

func TestConsentGate_WindowIsBounded(t *testing.T) {
	clock := time.Unix(0, 0)
	g := newTestConsentGate(&clock)
	g.windowSize = 64

	assert.False(t, g.feed(make([]byte, 1000)))
	assert.LessOrEqual(t, len(g.window), 64)

	clock = clock.Add(time.Second)
	assert.True(t, g.feed([]byte(consentMarker)), "marker within the bounded window still fires")
}

// Captured from CC 2.1.257 rendering the consent splash into a 160-col pty:
// Ink positions every word with a CSI cursor-column sequence, so the label is
// never contiguous in the raw stream.
const inkConsentFrame = "\x1b[3G\x1b[91m\x1b[1mWARNING:\x1b[12GLoading\x1b[20Gdevelopment\x1b[32Gchannels\x1b[22m\x1b[39m\r\r\n" +
	"\x1b[3G\x1b[37mChannels:\x1b[13Gplugin:telegram@local-yakov\x1b[39m\r\r\n\r\r\n" +
	"\x1b[3G\x1b[94m❯\x1b[5G\x1b[37m1.\x1b[8G\x1b[94mI\x1b[10Gam\x1b[13Gusing\x1b[19Gthis\x1b[24Gfor\x1b[28Glocal\x1b[34Gdevelopment\x1b[39m\r\r\n" +
	"\x1b[5G\x1b[37m2.\x1b[8G\x1b[39mExit\r\r\n\r\r\n" +
	"\x1b[3G\x1b[37m\x1b[3mEnter\x1b[9Gto\x1b[12Gconfirm\x1b[20G·\x1b[22GEsc\x1b[26Gto\x1b[29Gcancel\x1b[23m\x1b[39m\r\r\n\x1b[2C\x1b[4A"

func TestNormalizeTTY(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "I am using this for local development", want: "I am using this for local development"},
		{name: "ink cursor positioning", in: inkConsentFrame, want: "WARNING: Loading development channels Channels: plugin:telegram@local-yakov ❯ 1. I am using this for local development 2. Exit Enter to confirm · Esc to cancel "},
		{name: "osc and single-char escapes", in: "\x1b]0;title\x07a\x1b7b\x1b[?25lc", want: "a b c"},
		{name: "truncated csi at end", in: "abc\x1b[3", want: "abc "},
		{name: "runs collapse", in: "a \r\n\t  b", want: "a b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, string(normalizeTTY([]byte(tt.in))))
		})
	}
}

func TestConsentGate_MatchesInkRenderedSplash(t *testing.T) {
	clock := time.Unix(0, 0)
	g := newTestConsentGate(&clock)

	half := len(inkConsentFrame) / 2
	assert.False(t, g.feed([]byte(inkConsentFrame[:half])))
	clock = clock.Add(time.Second)
	assert.True(t, g.feed([]byte(inkConsentFrame[half:])), "label split by escape sequences AND across reads must still match")
}
