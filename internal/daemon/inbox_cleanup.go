package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
)

type InboxCleanup struct {
	store    *access.Store
	ttl      time.Duration
	interval time.Duration
}

// NewInboxCleanup returns nil when ttl == 0 (cleanup disabled).
func NewInboxCleanup(store *access.Store, ttl, interval time.Duration) *InboxCleanup {
	if ttl <= 0 {
		return nil
	}

	if interval <= 0 {
		interval = time.Hour
	}

	return &InboxCleanup{store: store, ttl: ttl, interval: interval}
}

func (ic *InboxCleanup) Run(ctx context.Context) {
	ic.sweepOnce()

	t := time.NewTicker(ic.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ic.sweepOnce()
		}
	}
}

func (ic *InboxCleanup) sweepOnce() {
	dir := ic.store.InboxDir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("inbox sweep readdir failed", "dir", dir, "err", err)
		}

		return
	}

	cutoff := time.Now().Add(-ic.ttl)

	var (
		removed int
		kept    int
		bytes   int64
	)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(cutoff) {
			kept++
			continue
		}

		full := filepath.Join(dir, e.Name())
		if err := os.Remove(full); err != nil {
			slog.Warn("inbox sweep remove failed", "path", full, "err", err)
			continue
		}

		removed++
		bytes += info.Size()
	}

	if removed > 0 {
		slog.Info("inbox sweep done", "removed", removed, "kept", kept, "bytes_freed", bytes, "ttl", ic.ttl)
	}
}
