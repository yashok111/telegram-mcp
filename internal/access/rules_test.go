package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRuleID_format(t *testing.T) {
	id1 := NewRuleID()
	id2 := NewRuleID()

	assert.Len(t, id1, 6)
	assert.Len(t, id2, 6)
	assert.NotEqual(t, id1, id2)

	for _, r := range id1 {
		assert.True(t, (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'),
			"non-hex char %q in %q", r, id1)
	}
}

func TestPermissionRule_jsonRoundTrip(t *testing.T) {
	r := PermissionRule{
		ID:          "abc123",
		Tool:        "Read",
		PathPattern: "/foo/**/*.go",
		Action:      RuleApprove,
		ExpiresAt:   1234567890,
		CreatedAt:   1234500000,
	}

	buf, err := json.Marshal(r)
	require.NoError(t, err)

	var got PermissionRule
	require.NoError(t, json.Unmarshal(buf, &got))
	assert.Equal(t, r, got)
}

func TestMatch_emptyRules_returnsNil(t *testing.T) {
	assert.Nil(t, Match(nil, "Read", "/foo/bar.go"))
	assert.Nil(t, Match([]PermissionRule{}, "Read", "/foo/bar.go"))
}

func TestMatch_exactToolBeatsWildcard(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "wild", Tool: "*", Action: RuleApprove, CreatedAt: now},
		{ID: "exact", Tool: "Read", Action: RuleApprove, CreatedAt: now},
	}

	got := Match(rules, "Read", "")
	require.NotNil(t, got)
	assert.Equal(t, "exact", got.ID)
}

func TestMatch_pathPatternBeatsToolOnly(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "toolonly", Tool: "Read", Action: RuleApprove, CreatedAt: now},
		{ID: "withpath", Tool: "Read", PathPattern: "/foo/*.go", Action: RuleApprove, CreatedAt: now},
	}

	got := Match(rules, "Read", "/foo/bar.go")
	require.NotNil(t, got)
	assert.Equal(t, "withpath", got.ID)
}

func TestMatch_mostRecentWinsOnTie(t *testing.T) {
	rules := []PermissionRule{
		{ID: "older", Tool: "Read", Action: RuleApprove, CreatedAt: 1000},
		{ID: "newer", Tool: "Read", Action: RuleDeny, CreatedAt: 2000},
	}

	got := Match(rules, "Read", "")
	require.NotNil(t, got)
	assert.Equal(t, "newer", got.ID)
}

func TestMatch_expiredRuleSkipped(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "expired", Tool: "Read", Action: RuleApprove, ExpiresAt: now - 1000, CreatedAt: now - 2000},
	}
	assert.Nil(t, Match(rules, "Read", ""))

	rules = append(rules, PermissionRule{
		ID: "fresh", Tool: "Read", Action: RuleDeny, ExpiresAt: now + 60_000, CreatedAt: now,
	})
	got := Match(rules, "Read", "")
	require.NotNil(t, got)
	assert.Equal(t, "fresh", got.ID)
}

func TestMatch_longerPatternRanksHigher(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "short", Tool: "Read", PathPattern: "/a/*", Action: RuleApprove, CreatedAt: now},
		{ID: "long", Tool: "Read", PathPattern: "/a/very/long/path/*.go", Action: RuleApprove, CreatedAt: now},
	}

	got := Match(rules, "Read", "/a/very/long/path/x.go")
	require.NotNil(t, got)
	assert.Equal(t, "long", got.ID)
}

func TestMatch_pathGlob_simple(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		match   bool
	}{
		{"star matches single segment", "/foo/*.go", "/foo/bar.go", true},
		{"star does not match across slash", "/foo/*.go", "/foo/sub/bar.go", false},
		{"exact path", "/foo/bar.go", "/foo/bar.go", true},
		{"exact mismatch", "/foo/bar.go", "/foo/baz.go", false},
	}

	now := time.Now().UnixMilli()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := []PermissionRule{{
				ID: "r", Tool: "Read", PathPattern: tt.pattern,
				Action: RuleApprove, CreatedAt: now,
			}}
			got := Match(rules, "Read", tt.path)
			if tt.match {
				assert.NotNil(t, got)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

func TestMatch_pathGlob_doubleStar(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		match   bool
	}{
		{"prefix + ** + suffix matches", "/foo/**/bar", "/foo/x/y/bar", true},
		{"prefix + ** + suffix direct", "/foo/**/bar", "/foo/bar", true},
		{"prefix + ** + suffix mismatch suffix", "/foo/**/bar", "/foo/x/y/baz", false},
		{"prefix + ** + suffix mismatch prefix", "/foo/**/bar", "/quux/x/bar", false},
		{"** as prefix matches anything ending", "**/bar.go", "/foo/x/y/bar.go", true},
		{"** as suffix matches anything starting", "/foo/**", "/foo/x/y/z", true},
		{"only **", "**", "/any/path/at/all", true},
		{"multiple ** segments", "/a/**/b/**/c", "/a/x/b/y/c", true},
		{"multiple ** mismatched order", "/a/**/b/**/c", "/a/c/b", false},
	}

	now := time.Now().UnixMilli()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := []PermissionRule{{
				ID: "r", Tool: "Read", PathPattern: tt.pattern,
				Action: RuleApprove, CreatedAt: now,
			}}
			got := Match(rules, "Read", tt.path)
			if tt.match {
				assert.NotNil(t, got, "expected match for pattern=%q path=%q", tt.pattern, tt.path)
			} else {
				assert.Nil(t, got, "expected no match for pattern=%q path=%q", tt.pattern, tt.path)
			}
		})
	}
}

func TestMatch_emptyPath_withPathPattern_noMatch(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "r", Tool: "Read", PathPattern: "/foo/*.go", Action: RuleApprove, CreatedAt: now},
	}
	assert.Nil(t, Match(rules, "Read", ""))
}

func TestMatch_emptyPath_withoutPathPattern_matches(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "r", Tool: "Bash", Action: RuleApprove, CreatedAt: now},
	}
	got := Match(rules, "Bash", "")
	require.NotNil(t, got)
	assert.Equal(t, "r", got.ID)
}

func TestMatch_wildcardToolMatchesAnything(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "any", Tool: "*", Action: RuleApprove, CreatedAt: now},
	}
	got := Match(rules, "Bash", "")
	require.NotNil(t, got)
	assert.Equal(t, "any", got.ID)

	got = Match(rules, "Edit", "/foo")
	require.NotNil(t, got)
	assert.Equal(t, "any", got.ID)
}

func TestMatch_toolMismatch_returnsNil(t *testing.T) {
	now := time.Now().UnixMilli()
	rules := []PermissionRule{
		{ID: "r", Tool: "Read", Action: RuleApprove, CreatedAt: now},
	}
	assert.Nil(t, Match(rules, "Bash", ""))
}

func TestPruneRules_dropsExpired(t *testing.T) {
	now := time.Now().UnixMilli()
	st := State{Rules: []PermissionRule{
		{ID: "a", Tool: "Read", ExpiresAt: now - 1000, CreatedAt: now - 2000},
		{ID: "b", Tool: "Read", ExpiresAt: now + 60_000, CreatedAt: now},
		{ID: "c", Tool: "Read", ExpiresAt: now - 1, CreatedAt: now - 2},
	}}

	changed := PruneRules(&st)
	assert.True(t, changed)
	assert.Len(t, st.Rules, 1)
	assert.Equal(t, "b", st.Rules[0].ID)
}

func TestPruneRules_keepsZeroExpiry(t *testing.T) {
	st := State{Rules: []PermissionRule{
		{ID: "forever", Tool: "Read", ExpiresAt: 0, CreatedAt: 1},
	}}

	changed := PruneRules(&st)
	assert.False(t, changed)
	assert.Len(t, st.Rules, 1)
}

func TestPruneRules_noop_whenAllFresh(t *testing.T) {
	now := time.Now().UnixMilli()
	st := State{Rules: []PermissionRule{
		{ID: "a", Tool: "Read", ExpiresAt: now + 60_000, CreatedAt: now},
	}}
	assert.False(t, PruneRules(&st))
	assert.Len(t, st.Rules, 1)
}

func TestPruneRules_emptyState(t *testing.T) {
	st := State{}
	assert.False(t, PruneRules(&st))
	assert.Empty(t, st.Rules)
}

func TestAddRule_fillsDefaults(t *testing.T) {
	st := State{}
	AddRule(&st, PermissionRule{Tool: "Read", Action: RuleApprove})

	require.Len(t, st.Rules, 1)
	added := st.Rules[0]
	assert.NotEmpty(t, added.ID)
	assert.Len(t, added.ID, 6)
	assert.NotZero(t, added.CreatedAt)
	assert.Equal(t, "Read", added.Tool)
	assert.Equal(t, RuleApprove, added.Action)
}

func TestAddRule_preservesExplicitIDAndCreatedAt(t *testing.T) {
	st := State{}
	AddRule(&st, PermissionRule{
		ID: "fixed1", Tool: "Edit", Action: RuleDeny, CreatedAt: 42,
	})

	require.Len(t, st.Rules, 1)
	assert.Equal(t, "fixed1", st.Rules[0].ID)
	assert.Equal(t, int64(42), st.Rules[0].CreatedAt)
}

func TestRevokeRule_byID(t *testing.T) {
	st := State{Rules: []PermissionRule{
		{ID: "keep1", Tool: "Read", Action: RuleApprove},
		{ID: "drop", Tool: "Edit", Action: RuleDeny},
		{ID: "keep2", Tool: "Bash", Action: RuleApprove},
	}}

	ok := RevokeRule(&st, "drop")
	assert.True(t, ok)
	require.Len(t, st.Rules, 2)
	assert.Equal(t, "keep1", st.Rules[0].ID)
	assert.Equal(t, "keep2", st.Rules[1].ID)
}

func TestRevokeRule_missing_returnsFalse(t *testing.T) {
	st := State{Rules: []PermissionRule{
		{ID: "a", Tool: "Read", Action: RuleApprove},
	}}

	ok := RevokeRule(&st, "nope")
	assert.False(t, ok)
	assert.Len(t, st.Rules, 1)
}

func TestRevokeRule_emptyState(t *testing.T) {
	st := State{}
	assert.False(t, RevokeRule(&st, "any"))
}

func TestClearRules_count(t *testing.T) {
	st := State{Rules: []PermissionRule{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}}

	n := ClearRules(&st)
	assert.Equal(t, 3, n)
	assert.Empty(t, st.Rules)
}

func TestClearRules_empty_returnsZero(t *testing.T) {
	st := State{}
	assert.Equal(t, 0, ClearRules(&st))
	assert.Empty(t, st.Rules)
}

func TestState_rulesPersistThroughSaveLoad(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, false)

	now := time.Now().UnixMilli()
	original := defaultState()
	original.Rules = []PermissionRule{
		{ID: "rule01", Tool: "Read", Action: RuleApprove, CreatedAt: now},
		{ID: "rule02", Tool: "Bash", PathPattern: "/tmp/**", Action: RuleDeny, ExpiresAt: now + 60_000, CreatedAt: now},
	}
	require.NoError(t, s.Save(original))

	got := s.Load()
	assert.Equal(t, original.Rules, got.Rules)

	// Sanity: omitempty means nil rules don't appear in the file.
	emptyDir := t.TempDir()
	s2 := NewStore(emptyDir, false)
	require.NoError(t, s2.Save(defaultState()))

	buf, err := os.ReadFile(filepath.Join(emptyDir, "access.json"))
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(buf, &raw))
	_, hasRules := raw["rules"]
	assert.False(t, hasRules, "nil Rules should be omitted via omitempty")
}
