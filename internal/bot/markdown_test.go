package bot

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscapeMarkdownV2(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"no specials", "hello world", "hello world"},
		{
			"every special once", `_*[]()~` + "`" + `>#+-=|{}.!\`,
			`\_\*\[\]\(\)\~\` + "`" + `\>\#\+\-\=\|\{\}\.\!\\`,
		},
		{"dot in sentence", "Hello, world.", `Hello, world\.`},
		{"unicode passthrough", "привет, мир!", `привет, мир\!`},
		{"url-ish text", "see https://x.io/page?a=1", `see https://x\.io/page?a\=1`},
		{"emoji passthrough", "🔥 hot _stuff_", `🔥 hot \_stuff\_`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, EscapeMarkdownV2(tt.in))
		})
	}
}

func TestEscapeMarkdownV2Code(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain text untouched", "func main() { return }", "func main() { return }"},
		{"underscore untouched", "snake_case_var", "snake_case_var"},
		{"backtick escaped", "use `markdown` here", `use \` + "`" + `markdown\` + "`" + ` here`},
		{"backslash escaped", `path\to\file`, `path\\to\\file`},
		{"backtick and backslash together", "`\\", "\\`\\\\"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, EscapeMarkdownV2Code(tt.in))
		})
	}
}

func TestMdCode(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", "``"},
		{"hex id passthrough", "ff00aa", "`ff00aa`"},
		{"underscore not escaped inside", "shim_id", "`shim_id`"},
		{"backtick escaped inside", "a`b", "`a\\`b`"},
		{"backslash escaped inside", `a\b`, "`a\\\\b`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MdCode(tt.in))
		})
	}
}
