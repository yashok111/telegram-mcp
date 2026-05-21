package daemon

import "time"

// gcPerUserLocked drops keys from perUser whose stamps are all older than
// cutoff, EXCEPT the keepKey just touched by the caller. The caller already
// updated its own entry; this sweep amortizes cleanup of every other user's
// keys so the map size stays bounded by recently-active users. Must be
// called with the runner's mu held.
func gcPerUserLocked(perUser map[string][]time.Time, keepKey string, cutoff time.Time) {
	for key, stamps := range perUser {
		if key == keepKey {
			continue
		}

		stillFresh := false

		for _, t := range stamps {
			if t.After(cutoff) {
				stillFresh = true
				break
			}
		}

		if !stillFresh {
			delete(perUser, key)
		}
	}
}
