// Package chunk splits long messages for Telegram's 4096-character cap.
// Mirrors the TS plugin: 'length' = hard cut at limit, 'newline' = prefer
// paragraph then line then space boundaries (only when boundary is past the
// halfway mark).
//
// The limit is applied to byte length, not UTF-16 code-units (Telegram's
// authoritative measure). For ASCII-dominant payloads they match; for
// emoji-dense text the byte budget is more conservative than Telegram's
// own cap, so we never overshoot. Newline mode cuts on ASCII separators
// ("\n\n", "\n", " "), which are rune-boundary-safe. Length mode does a
// hard byte-cut and can split a multi-byte rune — callers needing rune
// safety should use Newline (the default for free-form text).
package chunk

import "strings"

type Mode string

const (
	Length  Mode = "length"
	Newline Mode = "newline"
)

const MaxChunkLimit = 4096

func Split(text string, limit int, mode Mode) []string {
	if limit <= 0 || limit > MaxChunkLimit {
		limit = MaxChunkLimit
	}

	if len(text) <= limit {
		return []string{text}
	}

	var out []string

	rest := text
	for len(rest) > limit {
		cut := limit
		if mode == Newline {
			para := strings.LastIndex(rest[:limit], "\n\n")
			line := strings.LastIndex(rest[:limit], "\n")

			space := strings.LastIndex(rest[:limit], " ")
			switch {
			case para > limit/2:
				cut = para
			case line > limit/2:
				cut = line
			case space > 0:
				cut = space
			}
		}

		out = append(out, rest[:cut])
		rest = strings.TrimLeft(rest[cut:], "\n")
	}

	if rest != "" {
		out = append(out, rest)
	}

	return out
}
