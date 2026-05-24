package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestResolveAdminMutateConfig(t *testing.T) {
	tests := []struct {
		name     string
		deny     string
		rate     string
		ttl      string
		wantDeny []string
		wantRate int
		wantTTL  time.Duration
		setDeny  bool
		setRate  bool
		setTTL   bool
	}{
		{name: "defaults", wantDeny: nil, wantRate: 60, wantTTL: 5 * time.Minute},
		{name: "denylist csv trimmed", setDeny: true, deny: " approve_pairing , add_allow ,broadcast_message", wantDeny: []string{"approve_pairing", "add_allow", "broadcast_message"}, wantRate: 60, wantTTL: 5 * time.Minute},
		{name: "rate parsed", setRate: true, rate: "10", wantRate: 10, wantTTL: 5 * time.Minute},
		{name: "rate invalid keeps default", setRate: true, rate: "abc", wantRate: 60, wantTTL: 5 * time.Minute},
		{name: "rate zero keeps default", setRate: true, rate: "0", wantRate: 60, wantTTL: 5 * time.Minute},
		{name: "ttl parsed", setTTL: true, ttl: "2m", wantRate: 60, wantTTL: 2 * time.Minute},
		{name: "ttl zero keeps default", setTTL: true, ttl: "0", wantRate: 60, wantTTL: 5 * time.Minute},
		{name: "ttl invalid keeps default", setTTL: true, ttl: "nonsense", wantRate: 60, wantTTL: 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv with empty when unset is fine — resolver treats "" as unset.
			if tt.setDeny {
				t.Setenv("TELEGRAM_ADMIN_DENY_TOOLS", tt.deny)
			} else {
				t.Setenv("TELEGRAM_ADMIN_DENY_TOOLS", "")
			}

			if tt.setRate {
				t.Setenv("TELEGRAM_ADMIN_MUTATE_RATE_PER_HOUR", tt.rate)
			} else {
				t.Setenv("TELEGRAM_ADMIN_MUTATE_RATE_PER_HOUR", "")
			}

			if tt.setTTL {
				t.Setenv("TELEGRAM_ADMIN_PENDING_TTL", tt.ttl)
			} else {
				t.Setenv("TELEGRAM_ADMIN_PENDING_TTL", "")
			}

			deny, rate, ttl := resolveAdminMutateConfig()
			assert.Equal(t, tt.wantDeny, deny)
			assert.Equal(t, tt.wantRate, rate)
			assert.Equal(t, tt.wantTTL, ttl)
		})
	}
}
