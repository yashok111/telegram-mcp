package daemon

import "sync"

// recordingSink is the shared test EventSink for the daemon package's
// event-emit tests (spawn/bg/handlers/errorburst). Defined once here so the
// parallel emit-site test files don't each redeclare a fake and collide.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = append(s.events, e)
}

// typeCount counts captured events of a given Type.
func (s *recordingSink) typeCount(t string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int

	for _, e := range s.events {
		if e.Type == t {
			n++
		}
	}

	return n
}
