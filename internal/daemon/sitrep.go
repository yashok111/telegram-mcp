package daemon

import (
	"context"
	"log/slog"
	"time"
)

// SitrepTicker fires Fire on a fixed interval. The daemon wires Fire to push
// ipc.NotifyAdminSitrep to the connected admin-agent, which then produces a
// digest DM to the owner. Interval <= 0 (or nil Fire) disables: Run returns
// immediately. Lifecycle is ctx-driven — Run exits on ctx.Done().
type SitrepTicker struct {
	interval time.Duration
	fire     func()
}

func NewSitrepTicker(interval time.Duration, fire func()) *SitrepTicker {
	return &SitrepTicker{interval: interval, fire: fire}
}

func (s *SitrepTicker) Run(ctx context.Context) {
	if s.interval <= 0 || s.fire == nil {
		slog.Info("sitrep ticker disabled", "interval", s.interval)
		return
	}

	t := time.NewTicker(s.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.fire()
		}
	}
}
