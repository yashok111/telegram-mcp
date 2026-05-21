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
			window := rest[:limit]

			// Try boundaries in descending preference. Short-circuit so we
			// don't scan the same prefix three times when the first hit
			// already qualifies.
			if para := strings.LastIndex(window, "\n\n"); para > limit/2 {
				cut = para
			} else if line := strings.LastIndex(window, "\n"); line > limit/2 {
				cut = line
			} else if space := strings.LastIndex(window, " "); space > 0 {
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
