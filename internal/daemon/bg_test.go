package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBgRunner_EmptyList(t *testing.T) {
	r := NewBgRunner(DefaultBgConfig())
	assert.Empty(t, r.List())
}

func TestBgRunner_CancelUnknown(t *testing.T) {
	r := NewBgRunner(DefaultBgConfig())
	assert.ErrorIs(t, r.Cancel("nope"), ErrTaskNotFound)
}

func TestBgRunner_ReserveSlotEnforcesMaxParallel(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 2, RatePerHourPerUser: 99})
	id1, err := r.reserveSlot("u1")
	require.NoError(t, err)
	id2, err := r.reserveSlot("u1")
	require.NoError(t, err)
	_, err = r.reserveSlot("u2")
	require.ErrorIs(t, err, ErrTooManyBgTasks)
	assert.NotEqual(t, id1, id2)
}

func TestBgRunner_ReserveSlotRateLimitsPerUser(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 99, RatePerHourPerUser: 2})
	_, err := r.reserveSlot("u1")
	require.NoError(t, err)
	r.releaseSlot(mustReserve(t, r, "u1"), BgStatusDone)
	_, err = r.reserveSlot("u1")
	require.ErrorIs(t, err, ErrRateLimited)
	_, err = r.reserveSlot("u2")
	require.NoError(t, err)
}

func TestBgRunner_ReleaseSlotFreesParallelSlot(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 1, RatePerHourPerUser: 99})
	id, err := r.reserveSlot("u1")
	require.NoError(t, err)
	_, err = r.reserveSlot("u1")
	require.ErrorIs(t, err, ErrTooManyBgTasks)
	r.releaseSlot(id, BgStatusDone)
	_, err = r.reserveSlot("u1")
	require.NoError(t, err)
}

func TestBgRunner_TaskIDsAreUnique(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 1000, RatePerHourPerUser: 1000})
	seen := map[string]bool{}

	for range 100 {
		id, err := r.reserveSlot("u1")
		require.NoError(t, err)
		assert.False(t, seen[id], "duplicate id %s", id)
		seen[id] = true
	}
}

func TestBgRunner_DefaultsAppliedForZeroValues(t *testing.T) {
	r := NewBgRunner(BgConfig{})
	assert.Equal(t, 3, r.cfg.MaxParallel)
	assert.Equal(t, "claude", r.cfg.ClaudeBin)
	assert.Positive(t, int64(r.cfg.Timeout))
	assert.Positive(t, int64(r.cfg.EditThrottle))
	assert.Equal(t, 10, r.cfg.RatePerHourPerUser)
}

func TestBgRunner_ListReflectsReserve(t *testing.T) {
	r := NewBgRunner(BgConfig{MaxParallel: 5, RatePerHourPerUser: 99})
	id, err := r.reserveSlot("u1")
	require.NoError(t, err)

	infos := r.List()
	require.Len(t, infos, 1)
	assert.Equal(t, id, infos[0].ID)
	assert.Equal(t, "u1", infos[0].UserID)
	assert.Equal(t, BgStatusRunning, infos[0].Status)
}

func mustReserve(t *testing.T, r *BgRunner, u string) string {
	t.Helper()

	id, err := r.reserveSlot(u)
	require.NoError(t, err)

	return id
}
