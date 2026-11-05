package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

func isAgyConversationID(id string) bool {
	return agyConversationIDPattern.MatchString(id)
}

// agyHome is agy's global state directory. It is shared by every
// agy process on the host; conversations are told apart by their id.
func agyHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

func agyStateDir(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".agy")
}

func agySessionIDFile(agentDirPath string) string {
	return filepath.Join(agyStateDir(agentDirPath), "conversation_id")
}

func agyThreadRefDir(agentDirPath string) string {
	return filepath.Join(agyStateDir(agentDirPath), "threads")
}

// agyKnownFile records every conversation id this agent has
// created, OneShot ones included. Reset can then remove all of them from
// agy's shared store, not only those a ref still points to.
func agyKnownFile(agentDirPath string) string {
	return filepath.Join(agyStateDir(agentDirPath), "conversations")
}

// agyExtrasDir holds, per conversation, the digest of the last
// per-call SystemPromptExtra the conversation was given (see
// agyResumeExtraNotice).
func agyExtrasDir(agentDirPath string) string {
	return filepath.Join(agyStateDir(agentDirPath), "extras")
}

func agyExtraFile(agentDirPath, convID string) string {
	return filepath.Join(agyExtrasDir(agentDirPath), convID)
}

func agyExtraDigest(extra string) string {
	sum := sha256.Sum256([]byte(extra))
	return hex.EncodeToString(sum[:])
}

// readAgyConversationExtra returns the digest recorded for convID, or ""
// when there is none (a conversation older than the record, or one whose
// record was lost).
func readAgyConversationExtra(agentDirPath, convID string) string {
	if !isAgyConversationID(convID) || agyRefuseStateSymlinks(agentDirPath) != nil {
		return ""
	}
	b, err := readAgyStateFile(agyExtraFile(agentDirPath, convID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeAgyConversationExtra records that convID has been given extra.
func writeAgyConversationExtra(agentDirPath, convID, extra string, logger *slog.Logger) {
	if !isAgyConversationID(convID) {
		return
	}
	digest := agyExtraDigest(extra)
	if readAgyConversationExtra(agentDirPath, convID) == digest {
		return
	}
	if err := agyRefuseStateSymlinks(agentDirPath); err != nil {
		logger.Warn("agy: refusing to write extra record", "err", err)
		return
	}
	path := agyExtraFile(agentDirPath, convID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("agy: mkdir for extra record failed", "path", path, "err", err)
		return
	}
	if err := agyRefuseFileSymlink(path); err != nil {
		logger.Warn("agy: refusing to write extra record", "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(digest), 0o644); err != nil {
		logger.Warn("agy: write extra record failed", "path", path, "err", err)
	}
}

// agyRefuseStateSymlinks refuses a symlinked .agy, threads or extras
// directory. The agent can write its own workspace, so a link there would
// otherwise redirect kojo's ref writes and cleanup to a path outside the
// agent dir. Same guard as the Gemini/OpenCode state dirs.
func agyRefuseStateSymlinks(agentDirPath string) error {
	return geminiRefuseSymlinks(agyStateDir(agentDirPath), agyThreadRefDir(agentDirPath), agyExtrasDir(agentDirPath))
}

// agyRefuseFileSymlink refuses a ref or ledger file that is a
// symlink, so a write or append cannot follow it out of the agent dir.
func agyRefuseFileSymlink(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

func readAgyStateFile(path string) ([]byte, error) {
	if err := agyRefuseFileSymlink(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func agyConversationRefPath(agentDirPath, sessionKey string) string {
	if sessionKey == "" {
		return agySessionIDFile(agentDirPath)
	}
	return filepath.Join(agyThreadRefDir(agentDirPath), "key-"+agentIDToUUID(sessionKey))
}

func readAgyConversationID(agentDirPath, sessionKey string) string {
	if err := agyRefuseStateSymlinks(agentDirPath); err != nil {
		return ""
	}
	path := agyConversationRefPath(agentDirPath, sessionKey)
	data, err := readAgyStateFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !isAgyConversationID(id) {
		_ = os.Remove(path)
		return ""
	}
	return id
}

func writeAgyConversationID(agentDirPath, sessionKey, id string, logger *slog.Logger) {
	if !isAgyConversationID(id) {
		logger.Warn("agy: refusing to persist non-UUID conversation id", "value", id)
		return
	}
	if err := agyRefuseStateSymlinks(agentDirPath); err != nil {
		logger.Warn("agy: refusing to write session ref", "err", err)
		return
	}
	path := agyConversationRefPath(agentDirPath, sessionKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("agy: mkdir for session ref failed", "path", path, "err", err)
		return
	}
	if err := agyRefuseFileSymlink(path); err != nil {
		logger.Warn("agy: refusing to write session ref", "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
		logger.Warn("agy: write session ref failed", "path", path, "err", err)
	}
}

// agyKnownMu serialises appends to and the reset of the per-agent
// known-conversations ledger.
var agyKnownMu sync.Mutex

func recordAgyConversation(agentDirPath, id string, logger *slog.Logger) {
	if !isAgyConversationID(id) {
		return
	}
	if err := agyRefuseStateSymlinks(agentDirPath); err != nil {
		logger.Warn("agy: refusing to update conversation ledger", "err", err)
		return
	}
	agyKnownMu.Lock()
	defer agyKnownMu.Unlock()
	path := agyKnownFile(agentDirPath)
	if data, err := readAgyStateFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == id {
				return
			}
		}
	} else if !os.IsNotExist(err) {
		logger.Warn("agy: refusing to read conversation ledger", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("agy: mkdir for conversation ledger failed", "err", err)
		return
	}
	if err := agyRefuseFileSymlink(path); err != nil {
		logger.Warn("agy: refusing to update conversation ledger", "err", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warn("agy: open conversation ledger failed", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(id + "\n"); err != nil {
		logger.Warn("agy: append conversation ledger failed", "err", err)
	}
}

// agyConversationIDPersisted reports whether id is still the resume
// target of any ref for this agent. A disposable (OneShot) conversation is
// only removed when no ref points at it.
func agyConversationIDPersisted(agentDirPath, id string) bool {
	if id == "" {
		return false
	}
	// Incomplete/unreadable state is not proof that a OneShot owns the
	// conversation exclusively. Keep it for a later reset in that case.
	if err := agyRefuseStateSymlinks(agentDirPath); err != nil {
		return true
	}
	if _, err := readAgyStateFile(agySessionIDFile(agentDirPath)); err != nil && !os.IsNotExist(err) {
		return true
	}
	if readAgyConversationID(agentDirPath, "") == id {
		return true
	}
	entries, err := os.ReadDir(agyThreadRefDir(agentDirPath))
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, rerr := readAgyStateFile(filepath.Join(agyThreadRefDir(agentDirPath), e.Name()))
		if rerr != nil {
			return true
		}
		if rerr == nil && strings.TrimSpace(string(body)) == id {
			return true
		}
	}
	return false
}

// agyConversationExists reports whether agy still has the conversation.
// agy keeps each one as ~/.gemini/antigravity-cli/conversations/<id>.db
// and silently starts a new conversation for an id it does not know, so
// the check is what lets a lost conversation get the recent-messages
// bootstrap of a fresh one. When the store cannot be located the id is
// trusted.
func agyConversationExists(id string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if _, err := os.Stat(dir); err != nil {
		return true
	}
	_, err = os.Stat(filepath.Join(dir, id+".db"))
	return err == nil
}

func agyCanResumeSession(agentID, sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	id := readAgyConversationID(agentDir(agentID), sessionKey)
	return id != "" && agyConversationExists(id)
}

func deleteAgyThreadRefStrict(agentID, sessionKey string) error {
	err := os.Remove(agyConversationRefPath(agentDir(agentID), sessionKey))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// removeAgyConversation deletes agy's on-disk state for one
// conversation and reports how many paths it removed. The rows agy keeps
// in conversation_summaries.db are left alone: that database is shared
// and owned by agy.
func removeAgyConversation(id string) (removed int, err error) {
	if !isAgyConversationID(id) {
		return 0, nil
	}
	home := agyHome()
	if home == "" {
		return 0, fmt.Errorf("agy: cannot locate conversation store: home directory unavailable")
	}
	paths := []string{
		filepath.Join(home, "conversations", id+".db"),
		filepath.Join(home, "conversations", id+".db-wal"),
		filepath.Join(home, "conversations", id+".db-shm"),
		filepath.Join(home, "conversations", id+".db-journal"),
		filepath.Join(home, "annotations", id+".pbtxt"),
		filepath.Join(home, "presence", id+".lock"),
		filepath.Join(home, "brain", id),
	}
	for _, p := range paths {
		if _, serr := os.Lstat(p); serr != nil {
			if !os.IsNotExist(serr) && err == nil {
				err = serr
			}
			continue
		}
		if rerr := os.RemoveAll(p); rerr != nil {
			if err == nil {
				err = rerr
			}
			continue
		}
		removed++
	}
	return removed, err
}

// clearAgySessionCounted drops every conversation this agent
// created, along with the refs and the ledger. It is used by the CLI
// session reset, by ResetData and by TruncateMemory (when the main
// conversation could not be trimmed).
func clearAgySessionCounted(agentID string) (filesRemoved int, err error) {
	return clearAgySessionsCounted(agentID, false)
}

// clearAgyKeyedSessionsCounted is clearAgySessionCounted except for the
// main conversation, which stays resumable along with its ref, its extra
// record and its ledger entry. TruncateMemory uses it after trimming the
// main conversation at a turn boundary.
func clearAgyKeyedSessionsCounted(agentID string) (filesRemoved int, err error) {
	return clearAgySessionsCounted(agentID, true)
}

func clearAgySessionsCounted(agentID string, keepMain bool) (filesRemoved int, err error) {
	dir := agentDir(agentID)
	if err := agyRefuseStateSymlinks(dir); err != nil {
		return 0, err
	}
	keep := ""
	if keepMain {
		body, rerr := readAgyStateFile(agySessionIDFile(dir))
		if rerr != nil && !os.IsNotExist(rerr) {
			return 0, rerr
		}
		if id := strings.TrimSpace(string(body)); isAgyConversationID(id) {
			keep = id
		}
	}
	// Any ref or ledger that exists but cannot be read may be the only
	// record of a conversation; fail before removing anything.
	ids := map[string]bool{}
	readRef := func(path string) error {
		body, err := readAgyStateFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if id := strings.TrimSpace(string(body)); isAgyConversationID(id) {
			ids[id] = true
		}
		return nil
	}
	if err := readRef(agySessionIDFile(dir)); err != nil {
		return 0, err
	}
	entries, rerr := os.ReadDir(agyThreadRefDir(dir))
	if rerr != nil && !os.IsNotExist(rerr) {
		return 0, rerr
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := readRef(filepath.Join(agyThreadRefDir(dir), e.Name())); err != nil {
			return 0, err
		}
	}
	agyKnownMu.Lock()
	defer agyKnownMu.Unlock()
	data, rerr := readAgyStateFile(agyKnownFile(dir))
	if rerr != nil && !os.IsNotExist(rerr) {
		return 0, rerr
	}
	for _, line := range strings.Split(string(data), "\n") {
		if id := strings.TrimSpace(line); isAgyConversationID(id) {
			ids[id] = true
		}
	}
	delete(ids, keep)
	var failed []string
	for id := range ids {
		n, rerr := removeAgyConversation(id)
		filesRemoved += n
		if rerr != nil {
			failed = append(failed, id)
			if err == nil {
				err = rerr
			}
		}
	}
	drop := []string{agyThreadRefDir(dir), agyKnownFile(dir)}
	if keep == "" {
		drop = append(drop, agySessionIDFile(dir), agyExtrasDir(dir))
	} else if entries, rerr := os.ReadDir(agyExtrasDir(dir)); rerr == nil {
		for _, e := range entries {
			if e.Name() != keep {
				drop = append(drop, filepath.Join(agyExtrasDir(dir), e.Name()))
			}
		}
	}
	for _, p := range drop {
		if rerr := os.RemoveAll(p); rerr != nil && !os.IsNotExist(rerr) && err == nil {
			err = rerr
		}
	}
	// Keep the ids whose state could not be removed in the ledger so the
	// next reset retries them; the refs are still dropped so no turn
	// resumes a half-deleted conversation. A kept main conversation stays
	// listed so a later reset removes it.
	ledger := failed
	if keep != "" {
		ledger = append(ledger, keep)
	}
	if len(ledger) > 0 {
		sort.Strings(ledger)
		if merr := os.MkdirAll(agyStateDir(dir), 0o755); merr == nil {
			if werr := os.WriteFile(agyKnownFile(dir), []byte(strings.Join(ledger, "\n")+"\n"), 0o644); werr != nil && err == nil {
				err = werr
			}
		}
	}
	return filesRemoved, err
}
