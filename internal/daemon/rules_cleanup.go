package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/yakov/telegram-mcp/internal/access"
)

type RulesCleanup struct {
	store    *access.Store
	interval time.Duration
}

func NewRulesCleanup(store *access.Store, interval time.Duration) *RulesCleanup {
	if interval <= 0 {
		interval = time.Minute
	}

	return &RulesCleanup{store: store, interval: interval}
}

func (rc *RulesCleanup) Run(ctx context.Context) {
	t := time.NewTicker(rc.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rc.pruneOnce()
		}
	}
}

func (rc *RulesCleanup) pruneOnce() {
	var (
		pruned    bool
		remaining int
	)

	err := rc.store.Mutate(func(st *access.State) bool {
		if !access.PruneRules(st) {
			return false
		}

		pruned = true
		remaining = len(st.Rules)

		return true
	})
	if err != nil {
		slog.Error("rules cleanup save failed", "err", err)
		return
	}

	if pruned {
		slog.Info("rules cleanup pruned expired rules", "remaining", remaining)
	}
}
