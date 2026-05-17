package daemon

import "strconv"

// allocAlias returns the lowest unused positive integer alias of the form "sN".
// Caller must hold r.mu (write).
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
