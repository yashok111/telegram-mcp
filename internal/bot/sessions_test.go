package bot

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBotNoRouterView(t *testing.T) {
	b := &Bot{}
	// Should not panic when calling helpers with nil router view.
	out := b.renderShims(time.Now())
	assert.NotEmpty(t, out, "renderShims returned empty string; expected friendly fallback")
}
