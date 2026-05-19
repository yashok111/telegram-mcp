package bot

import "strings"

// markdownV2Special is the set of characters Telegram requires backslash-escaped
// in MarkdownV2 plain text per
// https://core.telegram.org/bots/api#markdownv2-style.
var markdownV2Special = "_*[]()~`>#+-=|{}.!\\"

// EscapeMarkdownV2 escapes every Telegram MarkdownV2 special character in s by
// prepending '\\'. Use this when the text is plain user content that must not
// be parsed as MarkdownV2 syntax. Pass the result to the reply tool with
// format="markdownv2".
//
// This is intentionally aggressive — it does NOT try to preserve existing
// MarkdownV2 formatting inside s. If the caller is composing intentional
// MarkdownV2 (bold, italic, code), escape only the dynamic plain-text fragments
// and concatenate; don't run the whole string through this function.
func EscapeMarkdownV2(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)

	for _, r := range s {
		if strings.ContainsRune(markdownV2Special, r) {
			b.WriteByte('\\')
		}

		b.WriteRune(r)
	}

	return b.String()
}

// EscapeMarkdownV2Code escapes characters that retain meaning inside a
// MarkdownV2 inline-code (“...“) or pre-formatted (```...```) block:
// only '\\' and '`' per the spec.
func EscapeMarkdownV2Code(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/16)

	for _, r := range s {
		if r == '\\' || r == '`' {
			b.WriteByte('\\')
		}

		b.WriteRune(r)
	}

	return b.String()
}

// MdCode wraps s in a MarkdownV2 inline-code span — backticks plus the spec's
// in-code escape (only '\\' and '`'). Telegram renders these as tap-to-copy on
// iOS/Android, which is the whole point of using it for ids that the user
// will paste into a follow-up command. Callers MUST send with ParseMode
// "MarkdownV2" and escape MarkdownV2 specials in the surrounding text — a
// stray '.' or '-' will trip the Bot API parser regardless of this fragment.
func MdCode(s string) string {
	return "`" + EscapeMarkdownV2Code(s) + "`"
}
