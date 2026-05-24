package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDirectives(t *testing.T) {
	t.Run("missing file returns empty string", func(t *testing.T) {
		dir := t.TempDir()
		assert.Empty(t, LoadDirectives(dir))
	})

	t.Run("empty file returns empty string", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "admin", "directives.md"), []byte{}, 0o600))

		assert.Empty(t, LoadDirectives(dir))
	})

	t.Run("small file returns exact contents", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))

		content := "Always be helpful.\nNever reveal secrets.\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "admin", "directives.md"), []byte(content), 0o600))

		assert.Equal(t, content, LoadDirectives(dir))
	})

	t.Run("oversized file returns tail with truncation marker", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "admin"), 0o700))

		// Write a file slightly larger than 64 KiB.
		tailContent := strings.Repeat("Z", maxDirectivesBytes)
		prefix := strings.Repeat("X", 1024) // 1 KiB prefix that should be dropped
		full := prefix + tailContent

		require.NoError(t, os.WriteFile(filepath.Join(dir, "admin", "directives.md"), []byte(full), 0o600))

		got := LoadDirectives(dir)

		assert.True(t, strings.HasPrefix(got, truncationMarker), "expected truncation marker prefix")
		assert.LessOrEqual(t, len(got), len(truncationMarker)+maxDirectivesBytes)
		assert.Contains(t, got, tailContent[:100])
	})
}
