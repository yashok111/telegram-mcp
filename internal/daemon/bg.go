package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

type BgTaskStatus string

const (
	BgStatusRunning   BgTaskStatus = "running"
	BgStatusDone      BgTaskStatus = "done"
	BgStatusFailed    BgTaskStatus = "failed"
	BgStatusCancelled BgTaskStatus = "cancelled"
)

type BgConfig struct {
	MaxParallel        int
	Timeout            time.Duration
	DefaultWorkdir     string
	RatePerHourPerUser int
	EditThrottle       time.Duration
	ClaudeBin          string
}

func DefaultBgConfig() BgConfig {
	return BgConfig{
		MaxParallel:        3,
		Timeout:            30 * time.Minute,
		RatePerHourPerUser: 10,
		EditThrottle:       5 * time.Second,
		ClaudeBin:          "claude",
	}
}

type BgTaskInfo struct {
	ID         string
	StartedAt  time.Time
	Workdir    string
	PromptHead string
	UserID     string
	ChatID     string
	Status     BgTaskStatus
}

type bgTask struct {
	info   BgTaskInfo
	cancel func()
}

type BgRunner struct {
	cfg BgConfig

	mu      sync.Mutex
	tasks   map[string]*bgTask
	perUser map[string][]time.Time
}

var (
	ErrTaskNotFound   = errors.New("task not found")
	ErrTooManyBgTasks = errors.New("too many concurrent /bg tasks")
	ErrRateLimited    = errors.New("rate limited: try again later")
	ErrEmptyPrompt    = errors.New("empty prompt")
)

func NewBgRunner(cfg BgConfig) *BgRunner {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 3
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}

	if cfg.RatePerHourPerUser <= 0 {
		cfg.RatePerHourPerUser = 10
	}

	if cfg.EditThrottle <= 0 {
		cfg.EditThrottle = 5 * time.Second
	}

	if cfg.ClaudeBin == "" {
		cfg.ClaudeBin = "claude"
	}

	return &BgRunner{
		cfg:     cfg,
		tasks:   map[string]*bgTask{},
		perUser: map[string][]time.Time{},
	}
}

func (r *BgRunner) List() []BgTaskInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]BgTaskInfo, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t.info)
	}

	return out
}

func (r *BgRunner) Cancel(id string) error {
	r.mu.Lock()
	t, ok := r.tasks[id]
	r.mu.Unlock()

	if !ok {
		return ErrTaskNotFound
	}

	t.cancel()

	return nil
}

func (r *BgRunner) reserveSlot(userID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.tasks) >= r.cfg.MaxParallel {
		return "", ErrTooManyBgTasks
	}

	now := time.Now()
	cutoff := now.Add(-time.Hour)
	stamps := r.perUser[userID]

	keep := stamps[:0]
	for _, t := range stamps {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}

	if len(keep) >= r.cfg.RatePerHourPerUser {
		r.perUser[userID] = keep
		return "", ErrRateLimited
	}

	r.perUser[userID] = append(keep, now)

	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}

	id := hex.EncodeToString(buf)
	r.tasks[id] = &bgTask{
		info: BgTaskInfo{
			ID:        id,
			StartedAt: now,
			UserID:    userID,
			Status:    BgStatusRunning,
		},
		cancel: func() {},
	}

	return id, nil
}

func (r *BgRunner) releaseSlot(id string, finalStatus BgTaskStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.tasks[id]; ok {
		t.info.Status = finalStatus

		delete(r.tasks, id)
	}
}
