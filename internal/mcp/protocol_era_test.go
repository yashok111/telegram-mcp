package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// Claude Code admits a channel server (inbound push + permission relay) only
// while the negotiated MCP protocol version predates 2026-07-28 — a newer
// version flips CC's "protocol era" to modern and the channel path is silently
// skipped (no error anywhere; inbound + permission_request just stop).
// mcp-go echoes whichever valid version the client asks for, so the day
// ValidProtocolVersions grows a 2026-07-28+ entry this server goes deaf.
// Verified against the CC 2.1.257 bundle (Q2n admission filter, je() era
// predicate). Bump this guard only together with a channel-protocol migration.
const modernProtocolEra = "2026-07-28"

func TestProtocolVersionStaysInLegacyChannelEra(t *testing.T) {
	require.Less(t, mcp.LATEST_PROTOCOL_VERSION, modernProtocolEra)

	for _, v := range mcp.ValidProtocolVersions {
		require.Less(t, v, modernProtocolEra, "mcp-go now negotiates a modern-era protocol version; CC will drop the channel")
	}
}
