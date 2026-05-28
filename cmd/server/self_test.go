package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFixtureSession(t *testing.T, dir string, ccPID int) {
	t.Helper()

	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))

	payload := map[string]any{
		"alias":          "s3",
		"shim_id":        "abc123def456",
		"shim_id_prefix": "abc123de",
		"cc_pid":         ccPID,
		"shim_pid":       ccPID + 1,
		"cc_session_id":  "diag-sid",
		"workdir":        "/home/u/repo",
		"label":          "demo",
		"started_at":     time.Now().UTC().Format(time.RFC3339),
		"mode":           "shim",
	}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, strconv.Itoa(ccPID)+".json"), raw, 0o600))
}

// writePeerSession writes a session file with caller-provided alias / shim_pid /
// workdir, so peer-discovery tests can independently control liveness
// (shim_pid → /proc/<pid> existence) and identity per file.
func writePeerSession(t *testing.T, dir string, ccPID, shimPID int, alias, workdir string) {
	t.Helper()

	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))

	payload := map[string]any{
		"alias":          alias,
		"shim_id":        alias + "-shimid-fullhex",
		"shim_id_prefix": alias + "pref",
		"cc_pid":         ccPID,
		"shim_pid":       shimPID,
		"workdir":        workdir,
		"started_at":     time.Now().UTC().Format(time.RFC3339),
		"mode":           "shim",
	}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, strconv.Itoa(ccPID)+".json"), raw, 0o600))
}

// nonexistentPID returns a PID that is guaranteed to be absent from /proc on
// Linux: 99_999_999 sits above the default pid_max of 4_194_304, so the kernel
// never allocates it and /proc/99999999 never exists. Used by tests to fake a
// crashed-shim session file.
const nonexistentPID = 99_999_999

// stubAliveSet swaps peerProcAlive to report any pid in the live set as a
// running telegram-mcp process. The original hook is restored on t.Cleanup,
// so tests stay isolated.
func stubAliveSet(t *testing.T, live ...int) {
	t.Helper()

	livePids := make(map[int]struct{}, len(live))
	for _, p := range live {
		livePids[p] = struct{}{}
	}

	orig := peerProcAlive
	peerProcAlive = func(pid int) bool {
		_, ok := livePids[pid]

		return ok
	}

	t.Cleanup(func() { peerProcAlive = orig })
}

func TestListLivePeers_skipsOwnPidAndIncludesLivePeer(t *testing.T) {
	dir := t.TempDir()
	ownPID := 11111
	peerShimPID := 22223
	stubAliveSet(t, peerShimPID)

	writePeerSession(t, dir, ownPID, ownPID+1, "s1", "/home/u/own")
	writePeerSession(t, dir, 22222, peerShimPID, "s2", "/home/u/peer")

	peers := listLivePeers(dir, ownPID)
	require.Len(t, peers, 1)
	assert.Equal(t, "s2", peers[0].Alias)
	assert.Equal(t, "/home/u/peer", peers[0].Workdir)
}

func TestListLivePeers_skipsStaleShimPID(t *testing.T) {
	dir := t.TempDir()
	aliveShim := 30100
	stubAliveSet(t, aliveShim)

	writePeerSession(t, dir, 30001, nonexistentPID, "ghost", "/home/u/stale")
	writePeerSession(t, dir, 30002, aliveShim, "alive", "/home/u/alive")

	peers := listLivePeers(dir, 99999)
	require.Len(t, peers, 1)
	assert.Equal(t, "alive", peers[0].Alias)
}

func TestListLivePeers_skipsRecycledPidNotOurs(t *testing.T) {
	// Recycled-PID defense: a session file's shim_pid may now be owned by an
	// unrelated process. peerProcAlive must reject it.
	dir := t.TempDir()
	stubAliveSet(t /* no live PIDs at all */)

	writePeerSession(t, dir, 31001, 31002, "recycled", "/home/u/recycled")

	peers := listLivePeers(dir, 99999)
	assert.Empty(t, peers, "shim_pid not confirmed as telegram-mcp must be excluded")
}

func TestListLivePeers_skipsCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	goodShim := 40100
	stubAliveSet(t, goodShim)

	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "bad.json"), []byte("{not json"), 0o600))
	writePeerSession(t, dir, 40001, goodShim, "good", "/home/u/good")

	peers := listLivePeers(dir, 99999)
	require.Len(t, peers, 1)
	assert.Equal(t, "good", peers[0].Alias)
}

func TestListLivePeers_skipsEmptyAlias(t *testing.T) {
	dir := t.TempDir()
	stubAliveSet(t, 50100)
	writePeerSession(t, dir, 50001, 50100, "", "/home/u/no-alias")

	peers := listLivePeers(dir, 99999)
	assert.Empty(t, peers)
}

func TestListLivePeers_skipsInvalidShimPID(t *testing.T) {
	dir := t.TempDir()
	stubAliveSet(t /* none */)
	writePeerSession(t, dir, 51001, 0, "zero-pid", "/home/u/zero")
	writePeerSession(t, dir, 51002, -1, "neg-pid", "/home/u/neg")

	assert.Empty(t, listLivePeers(dir, 99999))
}

func TestListLivePeers_missingSessionsDir(t *testing.T) {
	dir := t.TempDir()
	assert.Empty(t, listLivePeers(dir, 12345))
}

func TestListLivePeers_emptySessionsDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sessions"), 0o700))
	assert.Empty(t, listLivePeers(dir, 12345))
}

func TestListLivePeers_ignoresNonJSONFiles(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "README"), []byte("scratch"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(sessDir, "subdir.json"), 0o700))

	assert.Empty(t, listLivePeers(dir, 1))
}

func TestRenderSelfText_appendsPeersLine(t *testing.T) {
	dir := t.TempDir()
	ownPID := 70001
	peerShim := 70100
	stubAliveSet(t, peerShim)

	writeFixtureSession(t, dir, ownPID)
	writePeerSession(t, dir, 70002, peerShim, "s2", "/home/u/peer-a")

	got := renderSelfText(dir, func() int { return ownPID })
	assert.Contains(t, got, "Peers online:")
	assert.Contains(t, got, "@s2 (/home/u/peer-a)")
}

func TestRenderSelfText_omitsPeersLineWhenZeroLivePeers(t *testing.T) {
	dir := t.TempDir()
	ownPID := 80001

	stubAliveSet(t /* none alive */)

	writeFixtureSession(t, dir, ownPID)
	writePeerSession(t, dir, 80002, nonexistentPID, "ghost", "/home/u/dead")

	got := renderSelfText(dir, func() int { return ownPID })
	assert.NotContains(t, got, "Peers online")
}

func TestRenderSelfText_peersLineListsMultipleSortedByAlias(t *testing.T) {
	dir := t.TempDir()
	ownPID := 90001
	pidA, pidB, pidC := 90100, 90200, 90300
	stubAliveSet(t, pidA, pidB, pidC)

	writeFixtureSession(t, dir, ownPID)
	writePeerSession(t, dir, 90010, pidA, "s9", "/home/u/nine")
	writePeerSession(t, dir, 90020, pidB, "s2", "/home/u/two")
	writePeerSession(t, dir, 90030, pidC, "s5", "/home/u/five")

	got := renderSelfText(dir, func() int { return ownPID })
	idx2 := strings.Index(got, "@s2")
	idx5 := strings.Index(got, "@s5")
	idx9 := strings.Index(got, "@s9")

	require.NotEqual(t, -1, idx2)
	require.NotEqual(t, -1, idx5)
	require.NotEqual(t, -1, idx9)
	assert.Less(t, idx2, idx5, "@s2 must come before @s5")
	assert.Less(t, idx5, idx9, "@s5 must come before @s9")
}

func TestRenderSelfText_noPeersSectionOnFallback(t *testing.T) {
	dir := t.TempDir()
	// Own session file missing → fallback message; peer presence must not leak in.
	stubAliveSet(t, 91100)
	writePeerSession(t, dir, 91001, 91100, "s2", "/home/u/peer")
	got := renderSelfText(dir, func() int { return 99000 })

	assert.Contains(t, got, "no shim alias registered")
	assert.NotContains(t, got, "Peers online")
}

func TestPeerProcAlive_realLookupOnSelfRejectsTestBinary(t *testing.T) {
	// Sanity-check the production lookup: it must NOT classify the running
	// test binary (server.test) as a telegram-mcp process. Catches regressions
	// where the comm-string match is removed or weakened.
	assert.False(t, peerProcAlive(os.Getpid()))
}

func TestRunSelf_textMode_includesAliasAndWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "98765")
	writeFixtureSession(t, dir, 98765)

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	require.Equal(t, 0, code)

	body := out.String()
	assert.Contains(t, body, "@s3")
	assert.Contains(t, body, "/home/u/repo")
	assert.Contains(t, body, "abc123de")
	assert.Contains(t, body, "98765")
}

func TestRunSelf_textMode_missingFile_noAlias(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "11111")

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "no shim alias registered")
}

func TestRunSelf_textMode_ccPIDZero_noAlias(t *testing.T) {
	dir := t.TempDir()

	var out bytes.Buffer

	// Inject ccPIDFn=0 directly via renderSelfText to bypass the live PPID walk
	// (the test binary's parent may itself be "claude" when run inside CC).
	got := renderSelfText(dir, func() int { return 0 })
	out.WriteString(got)
	assert.Contains(t, out.String(), "no shim alias registered")
}

func TestRunSelf_hookMode_emitsSessionStartJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "33333")
	writeFixtureSession(t, dir, 33333)

	var out bytes.Buffer

	code := runSelf(dir, []string{"--hook"}, &out)
	require.Equal(t, 0, code)

	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}

	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	assert.Equal(t, "SessionStart", payload.HookSpecificOutput.HookEventName)
	assert.Contains(t, payload.HookSpecificOutput.AdditionalContext, "@s3")
}

func TestRunSelf_hookMode_includesSessionTitle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "33334")
	writeFixtureSession(t, dir, 33334)

	var out bytes.Buffer

	code := runSelf(dir, []string{"--hook"}, &out)
	require.Equal(t, 0, code)

	var payload struct {
		HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
	}

	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	title, ok := payload.HookSpecificOutput["sessionTitle"]
	require.True(t, ok, "sessionTitle must be present when an alias is registered")
	assert.Equal(t, "tg:@s3", title)
}

func TestRunSelf_hookMode_omitsSessionTitleWhenNoAlias(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "33335")
	// No session file written → no alias → sessionTitle key must be absent so
	// CC keeps the default title instead of blanking it.

	var out bytes.Buffer

	code := runSelf(dir, []string{"--hook"}, &out)
	require.Equal(t, 0, code)

	var payload struct {
		HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
	}

	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	_, ok := payload.HookSpecificOutput["sessionTitle"]
	assert.False(t, ok, "sessionTitle must be omitted when no shim alias is registered")
}

func TestRunSelf_statusline_emitsCompactAliasTag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "44444")
	writeFixtureSession(t, dir, 44444)

	var out bytes.Buffer

	code := runSelf(dir, []string{"--statusline"}, &out)
	require.Equal(t, 0, code)
	assert.Equal(t, "tg:@s3", out.String())
}

func TestRunSelf_statuslineBeatsHookWhenBothFlagsPresent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "66666")
	writeFixtureSession(t, dir, 66666)

	var out bytes.Buffer

	code := runSelf(dir, []string{"--hook", "--statusline"}, &out)
	require.Equal(t, 0, code)
	assert.Equal(t, "tg:@s3", out.String(), "--statusline takes precedence over --hook")
}

func TestRunSelf_statusline_missingFile_emptyOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CC_PID", "55555")

	var out bytes.Buffer

	code := runSelf(dir, []string{"--statusline"}, &out)
	require.Equal(t, 0, code)
	assert.Empty(t, out.String())
}

func TestRenderStatuslineText_ccPIDZero_empty(t *testing.T) {
	dir := t.TempDir()
	got := renderStatuslineText(dir, func() int { return 0 })
	assert.Empty(t, got)
}

func TestRenderStatuslineText_corruptJSON_empty(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "1.json"), []byte("not json"), 0o600))

	got := renderStatuslineText(dir, func() int { return 1 })
	assert.Empty(t, got)
}

func TestRenderStatuslineText_emptyAlias_empty(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "2.json"), []byte(`{"alias":"","cc_pid":2}`), 0o600))

	got := renderStatuslineText(dir, func() int { return 2 })
	assert.Empty(t, got)
}

func TestSelectModeSelfSubcommand(t *testing.T) {
	t.Setenv("TELEGRAM_DAEMON", "")
	assert.Equal(t, modeSelf, selectMode([]string{"telegram-mcp", "self"}))
}

func TestFindCCPID_envOverrideWins(t *testing.T) {
	t.Setenv("CC_PID", "42")
	assert.Equal(t, 42, findCCPID())
}

func TestFindCCPID_invalidEnvFallsThroughToWalk(t *testing.T) {
	t.Setenv("CC_PID", "garbage")
	// Walk may find a "claude" ancestor when the test runs inside Claude Code,
	// or hit pid 1 otherwise. Either way, no panic — just exercise the fallback.
	_ = findCCPID()
}

// writeSessionWithSID writes a session snapshot with caller-controlled alias and
// cc_session_id so the session-id resolution path can be exercised independently
// of the cc_pid filename.
func writeSessionWithSID(t *testing.T, dir string, ccPID int, alias, sid string) {
	t.Helper()

	sessDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o700))

	payload := map[string]any{
		"alias":          alias,
		"shim_id":        "abc123def456",
		"shim_id_prefix": "abc123de",
		"cc_pid":         ccPID,
		"shim_pid":       ccPID + 1,
		"cc_session_id":  sid,
		"workdir":        "/home/u/repo",
		"started_at":     time.Now().UTC().Format(time.RFC3339),
		"mode":           "shim",
	}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, strconv.Itoa(ccPID)+".json"), raw, 0o600))
}

func TestRenderSelfText_resolvesBySessionIDWhenPPIDFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "diag-sid")
	writeFixtureSession(t, dir, 55555) // cc_session_id == "diag-sid"

	// ccPIDFn=0 → the PPID path cannot resolve; only the session-id match can.
	got := renderSelfText(dir, func() int { return 0 })
	assert.Contains(t, got, "@s3")
	assert.Contains(t, got, "/home/u/repo")
}

func TestRenderSelfText_sessionIDTakesPriorityOverPPID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sid-B")
	writeSessionWithSID(t, dir, 100, "sA", "sid-A") // what a wrong PPID walk would pick
	writeSessionWithSID(t, dir, 200, "sB", "sid-B") // the session env actually points at

	got := renderSelfText(dir, func() int { return 100 })
	assert.Contains(t, got, "@sB")
	assert.NotContains(t, got, "@sA")
}

func TestRenderSelfText_ignoresSessionIDWhenNotUnderCC(t *testing.T) {
	dir := t.TempDir()
	// CLAUDECODE unset → the session-id env is not trusted even when present.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "diag-sid")
	writeFixtureSession(t, dir, 55555)

	got := renderSelfText(dir, func() int { return 0 })
	assert.Contains(t, got, "no shim alias registered")
}

func TestRenderStatuslineText_resolvesBySessionID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "diag-sid")
	writeFixtureSession(t, dir, 55555)

	got := renderStatuslineText(dir, func() int { return 0 })
	assert.Equal(t, "tg:@s3", got)
}

func TestRunSelf_textMode_resolvesBySessionIDWithoutPID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "diag-sid")
	writeFixtureSession(t, dir, 55555)

	var out bytes.Buffer

	code := runSelf(dir, []string{}, &out)
	require.Equal(t, 0, code)
	assert.Contains(t, out.String(), "@s3")
}

func TestReadProcPPID_self(t *testing.T) {
	pid := os.Getpid()
	ppid, err := readProcPPID(pid)
	require.NoError(t, err)
	assert.Equal(t, os.Getppid(), ppid)
}

func TestReadProcComm_self(t *testing.T) {
	comm, err := readProcComm(os.Getpid())
	require.NoError(t, err)
	assert.NotEmpty(t, comm)
}
