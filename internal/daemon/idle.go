package daemon

import (
	"context"
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
				}

				if time.Since(idleSince) >= i.timeout {
					i.onIdle()
					return
				}
			} else {
				idleSince = time.Time{}
			}
		}
	}
}
