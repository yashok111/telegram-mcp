package daemon

import "strconv"

// AdminAlias is the reserved alias for the persistent admin-agent shim. The
// admin-agent registers via hello with Role="admin"; allocAlias never returns
// this string because user shims only get "sN" (n>0). Centralizing the
// literal here keeps routing, mention resolution, and reservation guards
// referring to the same identifier.
const AdminAlias = "admin"

// allocAlias returns the lowest unused positive integer alias of the form "sN".
// Caller must hold r.mu (write). The "sN" scheme structurally cannot return
// AdminAlias, so user shims and the admin shim live in disjoint namespaces.
func (r *Router) allocAlias() string {
	for n := 1; ; n++ {
		alias := "s" + strconv.Itoa(n)
		if _, taken := r.aliases[alias]; !taken {
			return alias
		}
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
