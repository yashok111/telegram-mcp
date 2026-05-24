package admin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Event MIRRORS internal/daemon.Event — keep json tags identical (a
// wire-compat test guards drift). The admin package can't import daemon.
type Event struct {
	Type     string    `json:"type"`
	Severity string    `json:"severity"`
	TS       time.Time `json:"ts"`
	Subject  string    `json:"subject"`
	Detail   string    `json:"detail"`
}

const (
	eventsSub    = "admin/events.jsonl"
	maxEventLine = 1 << 20
)

// ReadRecentEvents returns the last n events from <stateDir>/admin/events.jsonl,
// oldest-first. Missing file => nil,nil (not an error). n<=0 => all.
func ReadRecentEvents(stateDir string, n int) ([]Event, error) {
	path := filepath.Join(stateDir, eventsSub)

	f, err := os.Open(path) //nolint:gosec // path is operator-controlled stateDir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("open events file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var events []Event

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLine)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			slog.Warn("admin events skipping malformed entry", "path", path, "err", err)
			continue
		}

		events = append(events, e)
	}

	if err := sc.Err(); err != nil {
		slog.Warn("admin events read stopped early; later entries dropped", "path", path, "err", err)
	}

	if n > 0 && len(events) > n {
		events = events[len(events)-n:]
	}

	return events, nil
}
