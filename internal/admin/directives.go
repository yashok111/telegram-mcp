package admin

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	directivesSub      = "admin/directives.md"
	maxDirectivesBytes = 64 << 10
	truncationMarker   = "[...truncated: showing last 64 KiB...]\n"
)

// LoadDirectives reads the operator's standing directives from
// <stateDir>/admin/directives.md. Returns "" if the file is missing or empty.
// Bounded read (cap at maxDirectivesBytes = 64KiB) — if larger, reads the
// LAST maxDirectivesBytes and prefixes with a truncation marker.
func LoadDirectives(stateDir string) string {
	path := filepath.Join(stateDir, directivesSub)

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}

		slog.Warn("admin directives stat failed", "path", path, "err", err)

		return ""
	}

	size := info.Size()
	if size == 0 {
		return ""
	}

	if size <= maxDirectivesBytes {
		data, err := os.ReadFile(path) //nolint:gosec // operator-controlled stateDir
		if err != nil {
			slog.Warn("admin directives read failed", "path", path, "err", err)

			return ""
		}

		return string(data)
	}

	// Oversized: read only the tail.
	f, err := os.Open(path) //nolint:gosec // operator-controlled stateDir
	if err != nil {
		slog.Warn("admin directives open failed", "path", path, "err", err)

		return ""
	}
	defer func() { _ = f.Close() }()

	offset := size - maxDirectivesBytes

	if _, err := f.Seek(offset, 0); err != nil {
		slog.Warn("admin directives seek failed", "path", path, "err", err)

		return ""
	}

	buf := make([]byte, maxDirectivesBytes)

	// io.ReadFull, not a bare Read: Read may return a short count on a single
	// call. ErrUnexpectedEOF means the file shrank between Stat and Seek — use
	// whatever we got.
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		slog.Warn("admin directives read tail failed", "path", path, "err", err)

		return ""
	}

	return truncationMarker + string(buf[:n])
}
