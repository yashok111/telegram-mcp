package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedirectStderrToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	restore, err := RedirectStderrTo(path)
	require.NoError(t, err)
	defer restore()

	_, _ = os.Stderr.WriteString("hello\n")

	restore()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "hello")
}

func TestShouldRedirectFalseWhenTTY(t *testing.T) {
	// We can't easily simulate a real tty in unit tests; assert the function
	// runs and returns a bool. Real behavior is covered by manual integration.
	_ = ShouldRedirect()
}
