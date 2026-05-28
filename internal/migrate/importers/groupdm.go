package importers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// groupdmsImporter migrates v0's groups.json plus per-group messages.jsonl
// into groupdms + groupdm_messages. Domain key: "groupdms". Depends on
// agents importer running first because group members are FK-validated.
type groupdmsImporter struct{}

func (groupdmsImporter) Domain() string { return "groupdms" }

// v0Group decodes one entry in v0's groupdms/groups.json.
type v0Group struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Members   []v0GroupMember `json:"members"`
	Cooldown  int             `json:"cooldown,omitempty"`
	Style     string          `json:"style,omitempty"`
	Venue     string          `json:"venue,omitempty"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt"`
}

// v0GroupMember mirrors v0's GroupMember. AgentName is intentionally
// dropped — v1's members_json carries only ids; UI display joins against
// agents on read.
type v0GroupMember struct {
	AgentID      string `json:"agentId"`
	AgentName    string `json:"agentName,omitempty"`
	NotifyMode   string `json:"notifyMode,omitempty"`
	DigestWindow int    `json:"digestWindow,omitempty"`
}

// v0GroupMessage decodes one line of a group's messages.jsonl.
type v0GroupMessage struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName,omitempty"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
}

func (groupdmsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "groupdms"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "groupdms")

	srcPaths, err := collectGroupDMsSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum groupdms sources: %w", err)
	}

	manifest, err := readV0(opts.V0Dir, filepath.Join(groupsDir(opts.V0Dir), "groups.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return markImported(ctx, st, "groupdms", 0, checksum)
		}
		return fmt.Errorf("read groups.json: %w", err)
	}

	var groups []v0Group
	if err := json.Unmarshal(manifest, &groups); err != nil {
		return fmt.Errorf("decode groups.json: %w", err)
	}

	// Pre-build a set of live agent ids so we can drop members that
	// reference agents that disappeared from agents.json — InsertGroupDM
	// rejects unknown member ids and we'd otherwise abort the entire
	// import on a single stale row. A ListAgents error is fatal: we'd
	// otherwise mark every member dead and silently strand transcripts.
	as, err := st.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	liveAgents := make(map[string]bool, len(as))
	for _, a := range as {
		liveAgents[a.ID] = true
	}

	count := 0
	for i := range groups {
		g := &groups[i]
		if g.ID == "" || g.Name == "" {
			logger.Warn("skipping group: missing id or name", "id", g.ID)
			continue
		}
		if err := importOneGroup(ctx, st, opts.V0Dir, g, liveAgents, logger); err != nil {
			return fmt.Errorf("group %s: %w", g.ID, err)
		}
		count++
	}
	return markImported(ctx, st, "groupdms", count, checksum)
}

func importOneGroup(ctx context.Context, st *store.Store, v0Dir string, g *v0Group, liveAgents map[string]bool, logger *slog.Logger) error {
	if existing, err := st.GetGroupDM(ctx, g.ID); err == nil && existing != nil {
		// Already inserted — fall through to the messages walk so a
		// crash between InsertGroupDM and the per-message loop still
		// converges on retry.
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	} else {
		// Preserve every member from groups.json, including ones whose
		// agent_id is no longer in agents.json. Dropping dead members
		// would mark the group as smaller than it was at v0 time and
		// would let a 2-person group with one departed member shrink
		// to a one-member oddity. Pass AllowDeadMembers=true so
		// InsertGroupDM does not reject the row.
		members := make([]store.GroupDMMember, 0, len(g.Members))
		hasDead := false
		for _, m := range g.Members {
			if m.AgentID == "" {
				continue
			}
			if !liveAgents[m.AgentID] {
				hasDead = true
				logger.Warn("group: keeping member with no live agent (importer escape hatch)",
					"group", g.ID, "agent", m.AgentID)
			}
			members = append(members, store.GroupDMMember{
				AgentID:      m.AgentID,
				NotifyMode:   m.NotifyMode,
				DigestWindow: m.DigestWindow,
			})
		}
		if len(members) == 0 {
			logger.Warn("group: skipping — empty members list in groups.json",
				"group", g.ID, "name", g.Name)
			return nil
		}
		created := parseRFC3339Millis(g.CreatedAt)
		updated := parseRFC3339Millis(g.UpdatedAt)
		if created == 0 {
			created = store.NowMillis()
		}
		if updated == 0 {
			updated = created
		}
		if _, err := st.InsertGroupDM(ctx, &store.GroupDMRecord{
			ID:       g.ID,
			Name:     g.Name,
			Members:  members,
			Style:    g.Style,
			Cooldown: g.Cooldown,
			Venue:    g.Venue,
		}, store.GroupDMInsertOptions{
			CreatedAt:        created,
			UpdatedAt:        updated,
			AllowDeadMembers: hasDead,
		}); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}

	return importGroupMessages(ctx, st, v0Dir, g.ID, liveAgents, logger)
}

func importGroupMessages(ctx context.Context, st *store.Store, v0Dir, groupID string, liveAgents map[string]bool, logger *slog.Logger) error {
	path := filepath.Join(groupsDir(v0Dir), groupID, "messages.jsonl")
	f, err := openV0(v0Dir, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	// Pre-load the current member set so per-message option selection
	// can be done without re-querying members_json on every line.
	memberSet, err := groupMembers(ctx, st, groupID)
	if err != nil {
		return fmt.Errorf("load members for %s: %w", groupID, err)
	}

	// Pre-load already-imported message ids for this group so the per-
	// row idempotency check is a map hit, not a SELECT-per-line.
	existing, err := loadExistingGroupMessageIDs(ctx, st, groupID)
	if err != nil {
		return fmt.Errorf("preload existing group ids: %w", err)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 32<<20)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m v0GroupMessage
		if err := json.Unmarshal(line, &m); err != nil {
			logger.Warn("groupdm messages: skipping malformed line",
				"group", groupID, "line", lineNo, "err", err)
			continue
		}
		if m.ID == "" {
			logger.Warn("groupdm messages: skipping line without id",
				"group", groupID, "line", lineNo)
			continue
		}
		if existing[m.ID] {
			continue
		}

		ts := parseRFC3339Millis(m.Timestamp)
		if ts == 0 {
			ts = store.NowMillis()
		}
		// Per-message option selection: only relax checks when needed.
		//   - empty / "user" sentinel → no relaxation needed; the
		//     store skips both checks for these.
		//   - alive + member        → strict path, no flags.
		//   - alive + non-member    → AllowNonMember only.
		//   - dead author           → AllowMissingAuthor (implies the
		//     membership check is skipped too).
		opts := store.GroupDMMessageInsertOptions{
			CreatedAt: ts,
			UpdatedAt: ts,
		}
		switch {
		case m.AgentID == "" || m.AgentID == store.UserSenderID:
			// no relaxation
		case !liveAgents[m.AgentID]:
			opts.AllowMissingAuthor = true
		case !memberSet[m.AgentID]:
			opts.AllowNonMember = true
		}
		if _, err := st.AppendGroupDMMessage(ctx, &store.GroupDMMessageRecord{
			ID:        m.ID,
			GroupDMID: groupID,
			AgentID:   m.AgentID,
			Content:   m.Content,
		}, opts); err != nil {
			return fmt.Errorf("append %s line %d: %w", m.ID, lineNo, err)
		}
		existing[m.ID] = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	return nil
}

// groupMembers reads members_json for groupID and returns it as a set
// keyed by agent_id. The caller uses it to decide whether a per-row
// AllowNonMember relaxation is necessary on each AppendGroupDMMessage
// call without scanning a 4-element JSON array per line.
func groupMembers(ctx context.Context, st *store.Store, groupID string) (map[string]bool, error) {
	g, err := st.GetGroupDM(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(g.Members))
	for _, m := range g.Members {
		out[m.AgentID] = true
	}
	return out, nil
}

// loadExistingGroupMessageIDs returns the set of groupdm_messages ids
// already stored for groupID. Tombstones are included (deleted_at IS
// NOT NULL) because the importer's idempotency contract is "do nothing
// when the id has been seen", regardless of subsequent soft-deletes.
func loadExistingGroupMessageIDs(ctx context.Context, st *store.Store, groupID string) (map[string]bool, error) {
	rows, err := st.DB().QueryContext(ctx,
		`SELECT id FROM groupdm_messages WHERE groupdm_id = ?`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}


