package access

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type RuleAction string

const (
	RuleApprove RuleAction = "approve"
	RuleDeny    RuleAction = "deny"
)

type PermissionRule struct {
	ID          string     `json:"id"`
	Tool        string     `json:"tool"`
	PathPattern string     `json:"path_pattern,omitempty"`
	Action      RuleAction `json:"action"`
	ExpiresAt   int64      `json:"expires_at,omitempty"`
	CreatedAt   int64      `json:"created_at"`
}

func NewRuleID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// Match returns the most-specific non-expired rule that applies to (tool, path),
// or nil. Specificity: exact tool (+2), path pattern present (+1 + len/10).
// Ties → highest CreatedAt wins.
func Match(rules []PermissionRule, tool, path string) *PermissionRule {
	now := time.Now().UnixMilli()

	var best *PermissionRule

	bestScore := -1

	for i := range rules {
		r := &rules[i]
		if r.ExpiresAt > 0 && r.ExpiresAt <= now {
			continue
		}

		if !toolMatches(r.Tool, tool) {
			continue
		}

		if r.PathPattern != "" && !pathMatches(r.PathPattern, path) {
			continue
		}

		score := 0
		if r.Tool != "" && r.Tool != "*" {
			score += 2
		}

		if r.PathPattern != "" {
			score += 1 + len(r.PathPattern)/10
		}

		if score > bestScore || (score == bestScore && best != nil && r.CreatedAt > best.CreatedAt) {
			best = r
			bestScore = score
		}
	}

	return best
}

func toolMatches(ruleTool, tool string) bool {
	if ruleTool == "" || ruleTool == "*" {
		return true
	}

	return ruleTool == tool
}

func pathMatches(pattern, path string) bool {
	if pattern == "" {
		return true
	}

	if path == "" {
		return false
	}

	if !strings.Contains(pattern, "**") {
		ok, err := filepath.Match(pattern, path)
		return err == nil && ok
	}

	parts := strings.Split(pattern, "**")
	if len(parts) == 2 {
		return strings.HasPrefix(path, parts[0]) && strings.HasSuffix(path, parts[1])
	}

	rest := path

	for i, p := range parts {
		if p == "" {
			continue
		}

		if i == 0 {
			if !strings.HasPrefix(rest, p) {
				return false
			}

			rest = rest[len(p):]

			continue
		}

		if i == len(parts)-1 {
			if !strings.HasSuffix(rest, p) {
				return false
			}

			continue
		}

		idx := strings.Index(rest, p)
		if idx < 0 {
			return false
		}

		rest = rest[idx+len(p):]
	}

	return true
}

// PruneRules drops rules whose ExpiresAt is set and elapsed. Returns true if anything was removed.
func PruneRules(st *State) bool {
	if len(st.Rules) == 0 {
		return false
	}

	now := time.Now().UnixMilli()
	kept := st.Rules[:0]
	changed := false

	for _, r := range st.Rules {
		if r.ExpiresAt > 0 && r.ExpiresAt <= now {
			changed = true
			continue
		}

		kept = append(kept, r)
	}

	st.Rules = kept

	return changed
}

func AddRule(st *State, r PermissionRule) {
	if r.ID == "" {
		r.ID = NewRuleID()
	}

	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}

	st.Rules = append(st.Rules, r)
}

func RevokeRule(st *State, id string) bool {
	for i, r := range st.Rules {
		if r.ID == id {
			st.Rules = slices.Delete(st.Rules, i, i+1)
			return true
		}
	}

	return false
}

func ClearRules(st *State) int {
	n := len(st.Rules)
	st.Rules = nil

	return n
}
