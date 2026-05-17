package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseMentions(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"no mention", "hello world", nil},
		{"single mention at start", "@s1 do the thing", []string{"s1"}},
		{"single mention mid-sentence", "ping @s2 please", []string{"s2"}},
		{"multiple mentions", "@s1 and @s2 work together", []string{"s1", "s2"}},
		{"dedupe repeats", "@s1 @s1 @s1", []string{"s1"}},
		{"broadcast keyword", "@all status check", []string{"all"}},
		{"all plus specific", "@all and also @s2", []string{"all", "s2"}},
		{"uppercase normalized to lowercase", "@S1 hi", []string{"s1"}},
		{"email not parsed as mention", "send to user@example.com now", nil},
		{"mention after punctuation", "result: @s1.", []string{"s1"}},
		{"mention with underscore and digit", "ping @abc_2 here", []string{"abc_2"}},
		{"mention with hyphen", "ping @shim-3 here", []string{"shim-3"}},
		{"empty content", "", nil},
		{"bare @ ignored", "this @ is alone", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMentions(tt.content)
			assert.Equal(t, tt.want, got)
		})
	}
}
