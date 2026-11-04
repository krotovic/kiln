package lsp

import (
	"fmt"
	"sort"
	"sync"
)

// MemoryScripts are stored scripts a host keeps in memory and replaces whole with Set (the scripts `kiln lsp`
// takes in its <scheme>/scripts notification).
type MemoryScripts struct {
	mu      sync.RWMutex
	scripts map[string]string
	version uint64
}

// NewMemoryScripts makes stored scripts of sources by ID (none when nil).
func NewMemoryScripts(sources map[string]string) *MemoryScripts {
	m := &MemoryScripts{}
	m.Set(sources)
	return m
}

// Set replaces the stored scripts with sources by ID, so sessions republish their diagnostics.
func (m *MemoryScripts) Set(sources map[string]string) {
	next := make(map[string]string, len(sources))
	for id, src := range sources {
		next[id] = src
	}
	m.mu.Lock()
	m.scripts = next
	m.version++
	m.mu.Unlock()
}

// Load gives a stored script's source.
func (m *MemoryScripts) Load(id string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src, ok := m.scripts[id]
	if !ok {
		return "", fmt.Errorf("no script %s", id)
	}
	return src, nil
}

// List lists the stored scripts' IDs, sorted.
func (m *MemoryScripts) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.scripts))
	for id := range m.scripts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Version grows with every Set.
func (m *MemoryScripts) Version() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.version
}
