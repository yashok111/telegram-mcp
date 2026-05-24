package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestResolveErrorBurstConfig(t *testing.T) {
	tests := []struct {
		name          string
		threshold     string
		window        string
		cooldown      string
		wantThreshold int
		wantWindow    time.Duration
		wantCooldown  time.Duration
		wantOK        bool
	}{
		{name: "defaults when unset", wantThreshold: 20, wantWindow: time.Minute, wantCooldown: 5 * time.Minute, wantOK: true},
		{name: "threshold override", threshold: "5", wantThreshold: 5, wantWindow: time.Minute, wantCooldown: 5 * time.Minute, wantOK: true},
		{name: "zero threshold disables", threshold: "0", wantOK: false},
		{name: "negative threshold disables", threshold: "-3", wantOK: false},
		{name: "invalid threshold falls back to default", threshold: "abc", wantThreshold: 20, wantWindow: time.Minute, wantCooldown: 5 * time.Minute, wantOK: true},
		{name: "window + cooldown override", threshold: "10", window: "30s", cooldown: "2m", wantThreshold: 10, wantWindow: 30 * time.Second, wantCooldown: 2 * time.Minute, wantOK: true},
		{name: "invalid window falls back", threshold: "10", window: "nope", wantThreshold: 10, wantWindow: time.Minute, wantCooldown: 5 * time.Minute, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TELEGRAM_ADMIN_ERRBURST_THRESHOLD", tt.threshold)
			t.Setenv("TELEGRAM_ADMIN_ERRBURST_WINDOW", tt.window)
			t.Setenv("TELEGRAM_ADMIN_ERRBURST_COOLDOWN", tt.cooldown)

			th, win, cd, ok := resolveErrorBurstConfig()

			assert.Equal(t, tt.wantOK, ok)

			if !tt.wantOK {
				return
			}

			assert.Equal(t, tt.wantThreshold, th)
			assert.Equal(t, tt.wantWindow, win)
			assert.Equal(t, tt.wantCooldown, cd)
		})
	}
}
