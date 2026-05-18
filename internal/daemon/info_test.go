package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestShimInfoIDPrefix(t *testing.T) {
	s := ShimInfo{ID: "abcdef012345"}
	assert.Equal(t, "abcdef01", s.IDPrefix())
}

func TestShimInfoIDPrefixShort(t *testing.T) {
	s := ShimInfo{ID: "abc"}
	assert.Equal(t, "abc", s.IDPrefix())
}

func TestShimInfoIdleFor(t *testing.T) {
	now := time.Now()
	s := ShimInfo{LastOutbound: now.Add(-2 * time.Minute)}
	assert.InDelta(t, (2 * time.Minute).Seconds(), s.IdleFor(now).Seconds(), 1.0)
}

func TestShimInfoIdleForZero(t *testing.T) {
	now := time.Now()
	s := ShimInfo{ConnectedAt: now.Add(-time.Minute)}
	assert.InDelta(t, time.Minute.Seconds(), s.IdleFor(now).Seconds(), 1.0)
}
