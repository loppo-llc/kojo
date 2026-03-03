package agent

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/loppo-llc/kojo/internal/session"
)

// harvestSession generates a summary of the completed session and stores it.
// summaryRef is set as soon as the summary session is created, allowing the caller to cancel it on timeout.
func (m *Manager) harvestSession(sess *session.Session, a *Agent, summaryRef *atomic.Pointer[session.Session]) {
	// Get scrollback output
	raw := sess.ScrollbackBytes()
	clean := session.StripANSI(raw)
	if len(clean) == 0 {
		return
	}
	if len(clean) > 8000 {
		clean = clean[len(clean)-8000:]
	}

	// Create summary session using --print (non-interactive)
	summarizePrompt := "Summarize this session output in 200 words or less. Focus on what was accomplished, decisions made, and open items:\n\n" + string(clean)

	summaryArgs := []string{"--print", summarizePrompt}
	summarySession, err := m.sessions.Create(a.Tool, a.WorkDir, summaryArgs, false, "")
	if err != nil {
		m.logger.Warn("failed to create summary session", "agent", a.ID, "err", err)
		// Fallback: save raw output as log
		m.saveLog(a.ID, sess.Info().ID, string(clean))
		return
	}
	summarySession.SetAgentID(a.ID)

	// Publish reference early so caller can cancel on timeout
	summaryRef.Store(summarySession)

	// Wait for summary session to complete
	<-summarySession.Done()
	summary := session.StripANSI(summarySession.ScrollbackBytes())

	m.saveLog(a.ID, sess.Info().ID, string(summary))

	// Update FTS5 index
	idx, err := m.openIndex(a.ID)
	if err == nil {
		idx.Insert("log", string(summary))
		idx.Close()
	}
}

func (m *Manager) saveLog(agentID, sessionID, content string) {
	logsDir := filepath.Join(m.agentDir(agentID), "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		m.logger.Warn("failed to create logs dir", "err", err)
		return
	}
	// Include session ID to prevent minute-precision collisions
	suffix := sessionID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	filename := time.Now().Format("2006-01-02_15-04") + "_" + suffix + ".md"
	path := filepath.Join(logsDir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		m.logger.Warn("failed to save log", "err", err)
	}
}
