package daemon

import (
	"log/slog"
	"strconv"

	"github.com/yakov/telegram-mcp/internal/access"
)

// AdminAlias is the reserved alias for the persistent admin-agent shim. The
// admin-agent registers via hello with Role="admin"; allocAlias never returns
// this string because user shims only get "sN" (n>0). Centralizing the
// literal here keeps routing, mention resolution, and reservation guards
// referring to the same identifier.
const AdminAlias = "admin"

// SetStickyAliasStore enables workdir/label-stable aliases. It loads the
// persisted reuse-key→alias bindings so a session reattaching to the same
// project gets the SAME @sN across reconnects and daemon restarts, and records
// the store for write-through whenever a new binding is minted. Wired once at
// daemon startup, before any shim connects; a nil store (the default in tests)
// leaves the legacy lowest-free-integer allocation in place.
func (r *Router) SetStickyAliasStore(store *access.Store) {
	if r == nil || store == nil {
		return
	}

	persisted := store.Load().AliasByKey // disk IO before taking the lock

	sticky := make(map[string]string, len(persisted))
	owner := make(map[string]string, len(persisted)) // alias → key, dedup guard

	for k, a := range persisted {
		if prev, dup := owner[a]; dup {
			slog.Warn("router: duplicate sticky alias on load — keeping first binding",
				"alias", a, "kept_key", prev, "dropped_key", k)

			continue
		}

		owner[a] = k
		sticky[k] = a
	}

	r.mu.Lock()
	r.aliasStore = store
	r.sticky = sticky
	r.mu.Unlock()
}

// stickyAliasKey returns the reuse-key a shim's alias binding is keyed on. It
// shares reuseKeyFor with Forum.resolveReuseKey so the topic-reuse key and the
// alias-stickiness key can never diverge. An empty result (no label, empty
// workdir) means no stable identity, so the caller falls back to plain
// allocation.
func stickyAliasKey(s *Shim) string {
	key, _ := reuseKeyFor(s.Label, s.Workdir)

	return key
}

// allocStickyAliasLocked picks the alias for a freshly-connecting shim. It
// returns (alias, mintKey); a non-empty mintKey means a new reuse-key→alias
// binding was created and the caller must persist it AFTER releasing r.mu.
// Caller holds r.mu (write).
//
//   - admin role → AdminAlias (never sticky; evicts any prior holder).
//   - no sticky store wired, or no stable key → legacy lowest-free allocation.
//   - stable key with a persisted alias that is currently free → reuse it.
//   - stable key whose persisted alias is held by another live shim (two
//     sessions in one project) → a transient fresh alias, binding untouched.
//   - stable key with no binding yet → allocate the lowest alias that is neither
//     live nor reserved by another project, and record the binding to persist.
func (r *Router) allocStickyAliasLocked(s *Shim) (string, string) {
	if s.Role == "admin" {
		return r.allocAliasForRole(s.Role), ""
	}

	key := stickyAliasKey(s)
	if key == "" || r.aliasStore == nil {
		return r.allocAlias(), ""
	}

	reserved := r.reservedAliasesLocked(key)

	if a := r.sticky[key]; a != "" {
		if _, taken := r.aliases[a]; !taken {
			return a, "" // persisted alias is free → reattach, already on disk
		}

		// Persisted alias is held by another live shim (a second session in the
		// same project): hand out a transient alias and leave the binding alone.
		return r.allocAliasAvoiding(reserved), ""
	}

	a := r.allocAliasAvoiding(reserved)

	if r.sticky == nil {
		r.sticky = map[string]string{}
	}

	r.sticky[key] = a

	return a, key
}

// reservedAliasesLocked returns the set of aliases bound to projects OTHER than
// exceptKey. Reserving them keeps an offline project's number from being handed
// to a different project, so the original reattaches to its own @sN. Caller
// holds r.mu.
func (r *Router) reservedAliasesLocked(exceptKey string) map[string]bool {
	if len(r.sticky) == 0 {
		return nil
	}

	reserved := make(map[string]bool, len(r.sticky))

	for k, a := range r.sticky {
		if k != exceptKey {
			reserved[a] = true
		}
	}

	return reserved
}

// persistStickyAlias write-throughs a minted reuse-key→alias binding to
// access.json. Called from Register WITHOUT r.mu held — store is captured under
// the lock by the caller so this never touches a Router field unsynchronized. A
// failed write only loses stickiness across a restart (the binding is
// recomputed), so it is logged, not fatal.
func persistStickyAlias(store *access.Store, key, alias string) {
	if store == nil {
		return
	}

	if err := store.Mutate(func(st *access.State) bool {
		if st.AliasByKey == nil {
			st.AliasByKey = map[string]string{}
		}

		if st.AliasByKey[key] == alias {
			return false
		}

		st.AliasByKey[key] = alias

		return true
	}); err != nil {
		slog.Error("router: persist sticky alias failed", "key", key, "alias", alias, "err", err)
	}
}

// allocAlias returns the lowest unused positive integer alias of the form "sN".
// Caller must hold r.mu (write). The "sN" scheme structurally cannot return
// AdminAlias, so user shims and the admin shim live in disjoint namespaces.
func (r *Router) allocAlias() string {
	return r.allocAliasAvoiding(nil)
}

// allocAliasAvoiding returns the lowest "sN" that is neither currently bound nor
// in the reserved set. reserved may be nil. Caller holds r.mu (write).
func (r *Router) allocAliasAvoiding(reserved map[string]bool) string {
	for n := 1; ; n++ {
		alias := "s" + strconv.Itoa(n)

		if _, taken := r.aliases[alias]; taken {
			continue
		}

		if reserved[alias] {
			continue
		}

		return alias
	}
}

// ResolveAlias returns the shim bound to the given alias, if any.
func (r *Router) ResolveAlias(alias string) (*Shim, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	id, ok := r.aliases[alias]
	if !ok {
		return nil, false
	}

	s, ok := r.shims[id]

	return s, ok
}

// AliasForShim returns the alias bound to shimID, or "" when the shim is not
// registered. Read-lock only; safe to call from handler paths.
func (r *Router) AliasForShim(shimID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.shimAlias[shimID]
}
