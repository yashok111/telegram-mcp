package daemon

import (
	"bytes"
	"io"
	"log/slog"
	"time"
)

// consentMarker is the confirm-button label of Claude Code's
// `--dangerously-load-development-channels` splash (CC bundle:
// confirmLabel:"I am using this for local development"). It is the one dialog
// a daemon-spawned CC is allowed to answer by itself.
const consentMarker = "I am using this for local development"

// consentGate decides when the spawner may press Enter into the child's pty.
//
// Earlier builds pressed Enter blindly 6× at startup. That also answered any
// other dialog CC happens to render first — the workspace-trust prompt for a
// never-opened `--in <dir>`, the "make auto mode the default" offer, the
// out-of-workdir read prompt (CC ≥2.1.257) — i.e. it granted trust to an
// arbitrary directory. The gate presses Enter only after the consent label
// has actually been drawn, at most maxConsentPresses times, and never again
// once the label stops re-rendering.
type consentGate struct {
	window     []byte
	presses    int
	lastPress  time.Time
	now        func() time.Time
	windowSize int
}

const (
	maxConsentPresses  = 6
	consentPressGap    = 500 * time.Millisecond
	consentWindowBytes = 8 << 10
)

func newConsentGate() *consentGate {
	return &consentGate{now: time.Now, windowSize: consentWindowBytes}
}

// feed appends a pty read and reports whether Enter should be pressed now.
// The rolling window survives a label split across reads; it is cleared on a
// press so the same frame is not answered twice.
func (g *consentGate) feed(chunk []byte) bool {
	if g.presses >= maxConsentPresses {
		return false
	}

	g.window = append(g.window, chunk...)
	if len(g.window) > g.windowSize {
		g.window = g.window[len(g.window)-g.windowSize:]
	}

	if !bytes.Contains(normalizeTTY(g.window), []byte(consentMarker)) {
		return false
	}

	t := g.now()
	if !g.lastPress.IsZero() && t.Sub(g.lastPress) < consentPressGap {
		return false
	}

	g.lastPress = t
	g.presses++
	g.window = g.window[:0]

	return true
}

// normalizeTTY flattens a raw pty stream into space-separated words so a
// label can be matched as plain text. Ink positions every word with a CSI
// cursor-column sequence (`I\x1b[10Gam\x1b[13Gusing…`, observed on CC
// 2.1.257), so the label never appears contiguously in the raw bytes: each
// escape sequence and control byte becomes one space, runs collapse to one.
func normalizeTTY(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	space := true

	emitSpace := func() {
		if !space {
			out = append(out, ' ')
			space = true
		}
	}

	for i := 0; i < len(raw); i++ {
		b := raw[i]
		switch {
		case b == 0x1b:
			i = skipEscape(raw, i)
			emitSpace()
		case b < 0x20 || b == 0x7f:
			emitSpace()
		case b == ' ':
			emitSpace()
		default:
			out = append(out, b)
			space = false
		}
	}

	return out
}

// skipEscape returns the index of the last byte of the escape sequence that
// starts at raw[i] (an ESC). CSI: ESC '[' params/intermediates, final byte in
// 0x40..0x7e. OSC: ESC ']' … BEL or ESC '\'. Anything else: ESC + one byte.
// A sequence cut off by the end of the buffer consumes to the end.
func skipEscape(raw []byte, i int) int {
	if i+1 >= len(raw) {
		return len(raw) - 1
	}

	switch raw[i+1] {
	case '[':
		for j := i + 2; j < len(raw); j++ {
			if raw[j] >= 0x40 && raw[j] <= 0x7e {
				return j
			}
		}

		return len(raw) - 1
	case ']':
		for j := i + 2; j < len(raw); j++ {
			if raw[j] == 0x07 {
				return j
			}

			if raw[j] == 0x1b && j+1 < len(raw) && raw[j+1] == '\\' {
				return j + 1
			}
		}

		return len(raw) - 1
	default:
		return i + 1
	}
}

// drainPty keeps the pty master read so the child's TUI never blocks, and
// answers the consent splash through the gate. Returns on read error (pty
// closed) — the caller owns the master fd.
func drainPty(p io.ReadWriter, gate *consentGate, pid int) {
	buf := make([]byte, 4096)
	for {
		n, err := p.Read(buf)
		if n > 0 && gate.feed(buf[:n]) {
			if _, werr := p.Write([]byte("\r")); werr != nil {
				slog.Warn("spawn consent: enter failed", "pid", pid, "err", werr)
			}
		}

		if err != nil {
			return
		}
	}
}
