package agent

import (
	"crypto/rand"
	"encoding/hex"
)

// generatePrefixedID returns a random ID with the given prefix (e.g. "ag_", "m_", "gd_").
func generatePrefixedID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
