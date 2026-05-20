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
		rulesPruned      bool
		pendingPruned    bool
		rulesRemaining   int
		pendingRemaining int
	)

	err := rc.store.Mutate(func(st *access.State) bool {
		rulesPruned = access.PruneRules(st)
		pendingPruned = access.PruneExpired(st)

		if !rulesPruned && !pendingPruned {
			return false
		}

		rulesRemaining = len(st.Rules)
		pendingRemaining = len(st.Pending)

		return true
	})
	if err != nil {
		slog.Error("state cleanup save failed", "err", err)
		return
	}

	if rulesPruned || pendingPruned {
		slog.Info("state cleanup pruned expired entries",
			"rules_pruned", rulesPruned,
			"rules_remaining", rulesRemaining,
			"pending_pruned", pendingPruned,
			"pending_remaining", pendingRemaining,
		)
	}
}
