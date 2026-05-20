package daemon

import (
	"os"
	"strings"
)

// prefixEnabled reports whether the daemon should prepend a `@sN:` source
// marker to shim-originated outbound messages. Disabled when env
// TELEGRAM_PREFIX_ALIAS is set to a falsy value ("0", "false", "no", "off",
// case-insensitive). Default: enabled.
func prefixEnabled() bool {
	v := strings.TrimSpace(os.Getenv("TELEGRAM_PREFIX_ALIAS"))
	if v == "" {
		return true
	}

	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	}

	return true
}

// formatTextPrefix returns the leading source marker for plain text or
// MarkdownV2 messages, e.g. "@s1: ". Returns "" when alias is empty.
// Aliases are always lowercase `s` + digits, so the result contains no
// MarkdownV2-special characters and needs no escape.
func formatTextPrefix(alias string) string {
	if alias == "" {
		return ""
	}

	return "@" + alias + ": "
}

// formatCaption returns the source marker used as a Telegram caption on file
// uploads (sendPhoto / sendDocument). Captions cannot contain a leading
// space, so we drop the trailing ": " of the text prefix and keep just "@sN".
// Returns "" when alias is empty.
func formatCaption(alias string) string {
	if alias == "" {
		return ""
	}

	return "@" + alias
}
