package agent

// getOrOpenIndex returns a cached MemoryIndex for the agent, opening one if needed.
// Uses double-checked locking to avoid holding the mutex during I/O.
func (m *Manager) getOrOpenIndex(agentID string) *MemoryIndex {
	m.memIndexesMu.Lock()
	if idx, ok := m.memIndexes[agentID]; ok {
		m.memIndexesMu.Unlock()
		return idx
	}
	m.memIndexesMu.Unlock()

	// Open outside lock
	idx, err := OpenMemoryIndex(agentID, m.logger, m.creds)
	if err != nil {
		m.logger.Warn("failed to open memory index", "agent", agentID, "err", err)
		return nil
	}

	// Re-check and store
	m.memIndexesMu.Lock()
	if existing, ok := m.memIndexes[agentID]; ok {
		m.memIndexesMu.Unlock()
		idx.Close() // another goroutine opened it first
		return existing
	}
	m.memIndexes[agentID] = idx
	m.memIndexesMu.Unlock()
	return idx
}

// closeIndex closes and removes the cached MemoryIndex for an agent.
func (m *Manager) closeIndex(agentID string) {
	m.memIndexesMu.Lock()
	idx, ok := m.memIndexes[agentID]
	if ok {
		delete(m.memIndexes, agentID)
	}
	m.memIndexesMu.Unlock()

	if ok {
		idx.Close()
	}
}

// ClearAllEmbeddings resets embeddings to NULL for all cached indexes.
// Called when the embedding model changes (dimensions may differ).
func (m *Manager) ClearAllEmbeddings() {
	m.memIndexesMu.Lock()
	defer m.memIndexesMu.Unlock()

	for _, idx := range m.memIndexes {
		idx.ClearEmbeddings()
	}
}

// CloseAllIndexes closes all cached MemoryIndex instances.
func (m *Manager) CloseAllIndexes() {
	m.memIndexesMu.Lock()
	defer m.memIndexesMu.Unlock()

	for id, idx := range m.memIndexes {
		idx.Close()
		delete(m.memIndexes, id)
	}
}
