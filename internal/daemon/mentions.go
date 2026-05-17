package daemon

import (
	"regexp"
	"strings"
)

// mentionRE matches @alias tokens preceded by start-of-string or a non-alphanumeric
// boundary, so "user@example.com" does NOT match "@example".
var mentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])@([A-Za-z0-9_-]+)`)

func parseMentions(content string) []string {
	matches := mentionRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))

	for _, m := range matches {
		tok := strings.ToLower(m[1])
		if _, dup := seen[tok]; dup {
			continue
		}

		seen[tok] = struct{}{}
		out = append(out, tok)
	}

	return out
}
