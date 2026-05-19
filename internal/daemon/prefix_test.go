package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrefixEnabled(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{name: "empty default true", env: "", want: true},
		{name: "zero false", env: "0", want: false},
		{name: "false lowercase", env: "false", want: false},
		{name: "no lowercase", env: "no", want: false},
		{name: "off lowercase", env: "off", want: false},
		{name: "OFF uppercase", env: "OFF", want: false},
		{name: "FALSE uppercase", env: "FALSE", want: false},
		{name: "No mixed", env: "No", want: false},
		{name: "one true", env: "1", want: true},
		{name: "true lowercase", env: "true", want: true},
		{name: "yes lowercase", env: "yes", want: true},
		{name: "on lowercase", env: "on", want: true},
		{name: "arbitrary non-falsy", env: "enabled", want: true},
		{name: "whitespace zero false", env: "  0  ", want: false},
		{name: "whitespace one true", env: "  1  ", want: true},
		{name: "whitespace only true", env: "   ", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TELEGRAM_PREFIX_ALIAS", tt.env)
			assert.Equal(t, tt.want, prefixEnabled())
		})
	}
}

func TestFormatTextPrefix(t *testing.T) {
	tests := []struct {
		name  string
		alias string
		want  string
	}{
		{name: "empty alias", alias: "", want: ""},
		{name: "s1", alias: "s1", want: "@s1: "},
		{name: "s99", alias: "s99", want: "@s99: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatTextPrefix(tt.alias))
		})
	}
}

func TestFormatCaption(t *testing.T) {
	tests := []struct {
		name  string
		alias string
		want  string
	}{
		{name: "empty alias", alias: "", want: ""},
		{name: "s1", alias: "s1", want: "@s1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatCaption(tt.alias))
		})
	}
}
