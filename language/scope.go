package language

import "sort"

// Scope is the set of names a host lets its expressions use. Compile refuses any other name, so a typo is caught
// when content is saved rather than when it runs.
type Scope struct {
	names map[string]struct{}
}

// NewScope declares names.
func NewScope(names ...string) Scope {
	return Scope{}.With(names...)
}

// With returns a copy of the scope with more names; the receiver is unchanged.
func (s Scope) With(names ...string) Scope {
	next := make(map[string]struct{}, len(s.names)+len(names))
	for n := range s.names {
		next[n] = struct{}{}
	}
	for _, n := range names {
		next[n] = struct{}{}
	}
	return Scope{names: next}
}

// Has reports whether name is declared.
func (s Scope) Has(name string) bool {
	_, ok := s.names[name]
	return ok
}

// Names lists the declared names, sorted.
func (s Scope) Names() []string {
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Env gives the names' values when a Program runs. A name the scope declared but the Env lacks is an evaluation
// error, so a host can leave out a value it doesn't have.
type Env interface {
	Lookup(name string) (Value, bool)
}

// MapEnv is an Env backed by a map.
type MapEnv map[string]Value

func (m MapEnv) Lookup(name string) (Value, bool) {
	v, ok := m[name]
	return v, ok
}
