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
	st := rc.store.Load()
	if !access.PruneRules(&st) {
		return
	}

	if err := rc.store.Save(st); err != nil {
		slog.Error("rules cleanup save failed", "err", err)
		return
	}

	slog.Info("rules cleanup pruned expired rules", "remaining", len(st.Rules))
}
