package daemon

import (
	"context"
	"log/slog"
	"time"
)

type IdleExit struct {
	router  *Router
	timeout time.Duration
	onIdle  func()
}

func NewIdleExit(r *Router, timeout time.Duration, onIdle func()) *IdleExit {
	return &IdleExit{router: r, timeout: timeout, onIdle: onIdle}
}

// Run blocks until ctx is done. When ConnectedCount drops to 0, starts a timer;
// if it elapses with the count still 0, calls onIdle and returns.
func (i *IdleExit) Run(ctx context.Context) {
	if i.timeout <= 0 {
		<-ctx.Done()
		return
	}

	check := time.NewTicker(i.timeout / 4)
	defer check.Stop()

	var idleSince time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			if i.router.ConnectedCount() == 0 {
				if idleSince.IsZero() {
					idleSince = time.Now()

					slog.Info("idle timer started", "timeout", i.timeout)
				}

				if time.Since(idleSince) >= i.timeout {
					// Re-check at expiry: a shim may have Register'd between the
					// earlier ConnectedCount() and now (microseconds-wide window,
					// but Register isn't atomic with the timer).
					if i.router.ConnectedCount() != 0 {
						slog.Info("idle expiry aborted — shim connected at expiry", "was_idle_for", time.Since(idleSince))
						idleSince = time.Time{}

						continue
					}

					slog.Info("idle timer elapsed — calling onIdle", "idle_for", time.Since(idleSince))
					i.onIdle()

					return
				}
			} else if !idleSince.IsZero() {
				slog.Info("idle timer cancelled — shim reconnected", "was_idle_for", time.Since(idleSince))
				idleSince = time.Time{}
			}
		}
	}
}
