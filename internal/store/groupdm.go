package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// validGroupDMStyles mirrors v0's GroupDMStyle enum. "efficient" is the
// token-saving default and the migration from v0 must round-trip both values.
var validGroupDMStyles = map[string]bool{
	"efficient":  true,
	"expressive": true,
}

// validGroupDMVenues mirrors v0's GroupDMVenue enum. "chatroom" is the
// default; "colocated" tells the LLM the members are co-present in real
// space and adjusts speech style accordingly.
var validGroupDMVenues = map[string]bool{
	"chatroom":  true,
	"colocated": true,
}

// validNotifyModes mirrors v0's NotifyMode enum on per-member options.
// Empty is treated as "realtime" on read so legacy groups round-trip.
// "muted" is included — v0 lets a member opt out of notifications entirely
// while still being able to read the group; dropping it here would cause
// the v0→v1 importer to reject otherwise-valid rows.
var validNotifyModes = map[string]bool{
	"":         true,
	"realtime": true,
	"digest":   true,
	"muted":    true,
}

// validGroupDMKinds enumerates the room kinds. "group" is the classic
// multi-member room; "dm" is a first-class 1:1 room (sugar over the same
// machinery, listed separately in the UI). Empty is normalized to "group".
// "thread" is a first-class parallel human↔agent thread room: always freshly
// created (no member-set dedup / dm_member_key), driven by a one-shot side
// conversation just like a single-agent "dm". Empty is normalized to "group".
var validGroupDMKinds = map[string]bool{
	"group":  true,
	"dm":     true,
	"thread": true,
}

// UserSenderID is the reserved agent_id for messages posted by the
// human user (the same constant exists in v0's internal/agent package).
// Posts under this id skip the member-of-group check and don't FK against
// the agents table — the repo recognizes the sentinel explicitly.
const UserSenderID = "user"

// GroupDMMember is one entry in members_json. Per-member notify/digest
// settings travel here so the v0→v1 importer doesn't lose calibration data
// before Phase 4 normalizes membership into a separate table.
type GroupDMMember struct {
	AgentID      string `json:"agent_id"`
	NotifyMode   string `json:"notify_mode,omitempty"`   // ""|"realtime"|"digest"
	DigestWindow int    `json:"digest_window,omitempty"` // seconds, only meaningful with digest mode
}

// GroupDMRecord mirrors the `groupdms` table.
//
// Caveat: members_json may contain agent_ids that have since been
// soft-deleted. The repo layer does not filter them out at read time
// because the etag canonical record covers the unfiltered list — masking
// dead members here would let a returned record's etag drift from the
// stored row. Phase 4 normalizes this into a groupdm_members FK table
// with a cascade-on-tombstone trigger; until then, callers that surface
// the member list to the UI should overlay a live-agents lookup.
type GroupDMRecord struct {
	ID       string
	Name     string
	Members  []GroupDMMember
	Style    string
	Cooldown int
	Venue    string
	// MaxHops caps agent-to-agent notification relay depth for this room.
	// 0 means "use the built-in default" (agent.defaultMaxHops).
	MaxHops int
	// Kind is "group" (default) or "dm" (first-class 1:1 room).
	Kind string

	Seq       int64
	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

// canonicalMemberSlice returns a deterministic projection of Members for
// etag hashing. Sorts by agent_id and includes the per-member options so
// changing a member's notify_mode invalidates caches even when the agent
// set is unchanged.
type canonicalMember struct {
	AgentID      string `json:"agent_id"`
	NotifyMode   string `json:"notify_mode"`
	DigestWindow int    `json:"digest_window"`
}

func canonicalizeMembers(in []GroupDMMember) []canonicalMember {
	out := make([]canonicalMember, len(in))
	for i, m := range in {
		mode := m.NotifyMode
		if mode == "" {
			mode = "realtime"
		}
		out[i] = canonicalMember{AgentID: m.AgentID, NotifyMode: mode, DigestWindow: m.DigestWindow}
	}
	slices.SortFunc(out, func(a, b canonicalMember) int { return cmp.Compare(a.AgentID, b.AgentID) })
	return out
}

type groupDMETagInput struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	MembersSorted []canonicalMember `json:"members_sorted"`
	Style         string            `json:"style"`
	Cooldown      int               `json:"cooldown"`
	Venue         string            `json:"venue"`
	MaxHops       int               `json:"max_hops,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	UpdatedAt     int64             `json:"updated_at"`
	DeletedAt     *int64            `json:"deleted_at"`
}

func computeGroupDMETag(r *GroupDMRecord) (string, error) {
	return CanonicalETag(r.Version, groupDMETagInput{
		ID:            r.ID,
		Name:          r.Name,
		MembersSorted: canonicalizeMembers(r.Members),
		Style:         r.Style,
		Cooldown:      r.Cooldown,
		Venue:         r.Venue,
		MaxHops:       r.MaxHops,
		Kind:          canonicalKind(r.Kind),
		UpdatedAt:     r.UpdatedAt,
		DeletedAt:     r.DeletedAt,
	})
}

// GroupDMInsertOptions follows the AgentInsertOptions shape.
type GroupDMInsertOptions struct {
	Now       int64
	Seq       int64
	CreatedAt int64
	UpdatedAt int64
	PeerID    string
	// AllowDeadMembers skips the "every member must be a live agent"
	// check. Reserved for the v0→v1 importer where a group's
	// groups.json may reference an agent_id that has since vanished
	// from agents.json — without this escape hatch, the entire group
	// (plus its transcript) would have to be dropped to import the
	// rest. API handlers MUST leave this false.
	AllowDeadMembers bool
}

// InsertGroupDM creates a new group DM. Member agent_ids are validated
// against the live agents table — silently accepting a bogus member would
// surface as an empty avatar/profile in the UI later, hard to trace.
func (s *Store) InsertGroupDM(ctx context.Context, rec *GroupDMRecord, opts GroupDMInsertOptions) (*GroupDMRecord, error) {
	if rec == nil {
		return nil, errors.New("store.InsertGroupDM: nil record")
	}
	if rec.ID == "" || rec.Name == "" {
		return nil, errors.New("store.InsertGroupDM: id/name required")
	}
	if rec.Style == "" {
		rec.Style = "efficient"
	}
	if !validGroupDMStyles[rec.Style] {
		return nil, fmt.Errorf("store.InsertGroupDM: invalid style %q", rec.Style)
	}
	if rec.Venue == "" {
		rec.Venue = "chatroom"
	}
	if !validGroupDMVenues[rec.Venue] {
		return nil, fmt.Errorf("store.InsertGroupDM: invalid venue %q", rec.Venue)
	}
	if rec.Cooldown < 0 {
		return nil, fmt.Errorf("store.InsertGroupDM: negative cooldown %d", rec.Cooldown)
	}
	if rec.MaxHops < 0 {
		return nil, fmt.Errorf("store.InsertGroupDM: negative max_hops %d", rec.MaxHops)
	}
	if rec.Kind == "" {
		rec.Kind = "group"
	}
	if !validGroupDMKinds[rec.Kind] {
		return nil, fmt.Errorf("store.InsertGroupDM: invalid kind %q", rec.Kind)
	}
	if len(rec.Members) == 0 {
		return nil, errors.New("store.InsertGroupDM: members required")
	}
	for i := range rec.Members {
		if !validNotifyModes[rec.Members[i].NotifyMode] {
			return nil, fmt.Errorf("store.InsertGroupDM: invalid notify_mode %q for %s",
				rec.Members[i].NotifyMode, rec.Members[i].AgentID)
		}
	}

	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}
	created := opts.CreatedAt
	if created == 0 {
		created = now
	}
	updated := opts.UpdatedAt
	if updated == 0 {
		updated = now
	}
	seq := opts.Seq
	if seq == 0 {
		seq = NextGlobalSeq()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if !opts.AllowDeadMembers {
		if err := verifyMembersAlive(ctx, tx, memberIDs(rec.Members)); err != nil {
			return nil, err
		}
	}

	out := *rec
	out.Members = dedupMembers(out.Members)
	out.Seq = seq
	out.Version = 1
	out.CreatedAt = created
	out.UpdatedAt = updated
	out.PeerID = opts.PeerID
	out.DeletedAt = nil
	out.ETag, err = computeGroupDMETag(&out)
	if err != nil {
		return nil, err
	}

	membersJSON, err := json.Marshal(out.Members)
	if err != nil {
		return nil, err
	}

	const q = `
INSERT INTO groupdms (
  id, name, members_json, style, cooldown, venue, max_hops, kind, dm_member_key,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.Name, string(membersJSON), out.Style, out.Cooldown, out.Venue, out.MaxHops, out.Kind,
		dmMemberKey(out.Kind, out.Members),
		out.Seq, out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
	); err != nil {
		return nil, fmt.Errorf("store.InsertGroupDM: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetGroupDM returns the live group DM by id.
//
// Caveat: members_json may contain agent_ids that have since been
// soft-deleted. The repo layer does not filter them out at read time
// because the etag canonical record covers the unfiltered list — masking
// dead members here would let a returned record's etag drift from the
// stored row. The proper fix is Phase 4's normalized groupdm_members table
// + cascade-on-tombstone trigger; until then, callers that surface the
// member list to the UI should overlay a live-agents lookup themselves.
func (s *Store) GetGroupDM(ctx context.Context, id string) (*GroupDMRecord, error) {
	const q = `
SELECT id, name, members_json, style, cooldown, venue, max_hops, kind, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM groupdms WHERE id = ? AND deleted_at IS NULL`
	rec, err := scanGroupDMRow(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListGroupDMs returns all live group DMs ordered by seq.
func (s *Store) ListGroupDMs(ctx context.Context) ([]*GroupDMRecord, error) {
	const q = `
SELECT id, name, members_json, style, cooldown, venue, max_hops, kind, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM groupdms WHERE deleted_at IS NULL ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GroupDMRecord
	for rows.Next() {
		rec, err := scanGroupDMRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// UpdateGroupDM applies mutate inside a tx with optimistic locking. Members
// added by mutate are re-validated against live agents.
func (s *Store) UpdateGroupDM(ctx context.Context, id, ifMatchETag string, mutate func(*GroupDMRecord) error) (*GroupDMRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT id, name, members_json, style, cooldown, venue, max_hops, kind, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM groupdms WHERE id = ? AND deleted_at IS NULL`
	cur, err := scanGroupDMRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return nil, ErrETagMismatch
	}

	next := *cur
	next.Members = append([]GroupDMMember(nil), cur.Members...)
	if err := mutate(&next); err != nil {
		return nil, err
	}

	// Reset immutable fields so a careless mutate() can't desync the
	// canonical record from the row.
	next.ID = cur.ID
	next.Seq = cur.Seq
	next.CreatedAt = cur.CreatedAt
	next.DeletedAt = cur.DeletedAt
	next.PeerID = cur.PeerID

	if next.Name == "" {
		return nil, errors.New("store.UpdateGroupDM: name required")
	}
	if next.Style == "" {
		next.Style = cur.Style
	}
	if !validGroupDMStyles[next.Style] {
		return nil, fmt.Errorf("store.UpdateGroupDM: invalid style %q", next.Style)
	}
	if next.Venue == "" {
		next.Venue = cur.Venue
	}
	if !validGroupDMVenues[next.Venue] {
		return nil, fmt.Errorf("store.UpdateGroupDM: invalid venue %q", next.Venue)
	}
	if next.Cooldown < 0 {
		return nil, fmt.Errorf("store.UpdateGroupDM: negative cooldown %d", next.Cooldown)
	}
	if next.MaxHops < 0 {
		return nil, fmt.Errorf("store.UpdateGroupDM: negative max_hops %d", next.MaxHops)
	}
	// Kind is immutable after creation — a dm can't be promoted to a group
	// (nor the reverse) because the UI partitions on it and the find-or-
	// create DM lookup keys on (kind, member set).
	next.Kind = cur.Kind
	if len(next.Members) == 0 {
		return nil, errors.New("store.UpdateGroupDM: members required")
	}
	for i := range next.Members {
		if !validNotifyModes[next.Members[i].NotifyMode] {
			return nil, fmt.Errorf("store.UpdateGroupDM: invalid notify_mode %q for %s",
				next.Members[i].NotifyMode, next.Members[i].AgentID)
		}
	}
	next.Members = dedupMembers(next.Members)
	// Only verify members the caller is *adding*. Existing members
	// imported via AllowDeadMembers (v0→v1 importer escape hatch) may
	// already reference soft-deleted agents — re-validating the entire
	// list would block routine edits (rename, style change, member
	// removal) on any group with a historical dead member. The
	// addedMemberIDs() projection is keyed on agent_id so reordering /
	// notify_mode tweaks of a previously-validated member don't trip
	// the alive check either.
	added := addedMemberIDs(cur.Members, next.Members)
	if len(added) > 0 {
		if err := verifyMembersAlive(ctx, tx, added); err != nil {
			return nil, err
		}
	}

	// Detect semantic no-op: returns the prior record unchanged when name,
	// style, venue, cooldown, and the canonicalized member projection all
	// match. Compares by canonical member shape so a notify_mode tweak or
	// digest_window edit *does* bump the version.
	if next.Name == cur.Name && next.Style == cur.Style &&
		next.Venue == cur.Venue && next.Cooldown == cur.Cooldown &&
		next.MaxHops == cur.MaxHops &&
		sameCanonicalMembers(next.Members, cur.Members) {
		return cur, nil
	}

	next.Version = cur.Version + 1
	next.UpdatedAt = NowMillis()
	next.ETag, err = computeGroupDMETag(&next)
	if err != nil {
		return nil, err
	}

	membersJSON, err := json.Marshal(next.Members)
	if err != nil {
		return nil, err
	}

	const upd = `
UPDATE groupdms SET
  name = ?, members_json = ?, style = ?, cooldown = ?, venue = ?, max_hops = ?,
  dm_member_key = ?,
  version = ?, etag = ?, updated_at = ?
WHERE id = ? AND etag = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, upd,
		next.Name, string(membersJSON), next.Style, next.Cooldown, next.Venue, next.MaxHops,
		dmMemberKey(next.Kind, next.Members),
		next.Version, next.ETag, next.UpdatedAt,
		id, cur.ETag,
	)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrETagMismatch
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}

// TouchGroupDM advances a group's updated_at (and version/etag) to reflect
// new activity — e.g. a freshly posted message — without changing any
// settings field. It exists because UpdateGroupDM short-circuits on a
// settings no-op (a message post changes nothing UpdateGroupDM tracks), so
// it would never bump updated_at; the room-list "last active" time reads
// updated_at, so without this the row would freeze at creation time across
// restarts. No-op when the group is missing or tombstoned.
//
// ts is the desired updated_at in epoch millis; a value not strictly
// greater than the current updated_at falls back to now (then to
// current+1) so the timestamp advances monotonically even under
// coarse-resolution (RFC3339 seconds) callers.
func (s *Store) TouchGroupDM(ctx context.Context, id string, ts int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT id, name, members_json, style, cooldown, venue, max_hops, kind, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM groupdms WHERE id = ? AND deleted_at IS NULL`
	cur, err := scanGroupDMRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	if ts <= cur.UpdatedAt {
		ts = NowMillis()
		if ts <= cur.UpdatedAt {
			ts = cur.UpdatedAt + 1
		}
	}
	cur.Version++
	cur.UpdatedAt = ts
	cur.ETag, err = computeGroupDMETag(cur)
	if err != nil {
		return err
	}

	const upd = `
UPDATE groupdms SET version = ?, etag = ?, updated_at = ?
WHERE id = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, upd, cur.Version, cur.ETag, cur.UpdatedAt, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SoftDeleteGroupDM tombstones the group DM. Idempotent. Recomputes etag.
func (s *Store) SoftDeleteGroupDM(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT id, name, members_json, style, cooldown, venue, max_hops, kind, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM groupdms WHERE id = ? AND deleted_at IS NULL`
	cur, err := scanGroupDMRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	now := NowMillis()
	cur.Version++
	cur.UpdatedAt = now
	cur.DeletedAt = &now
	newETag, err := computeGroupDMETag(cur)
	if err != nil {
		return err
	}

	const upd = `
UPDATE groupdms
   SET deleted_at = ?, updated_at = ?, version = ?, etag = ?
 WHERE id = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, upd, now, now, cur.Version, newETag, id); err != nil {
		return err
	}
	return tx.Commit()
}

func scanGroupDMRow(r rowScanner) (*GroupDMRecord, error) {
	var (
		rec       GroupDMRecord
		members   string
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.Name, &members, &rec.Style, &rec.Cooldown, &rec.Venue, &rec.MaxHops, &rec.Kind,
		&rec.Seq, &rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	rec.Kind = canonicalKind(rec.Kind)
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	if members != "" {
		if err := json.Unmarshal([]byte(members), &rec.Members); err != nil {
			return nil, fmt.Errorf("store: decode members_json for %s: %w", rec.ID, err)
		}
	}
	return &rec, nil
}

// dmMemberKey returns the canonical member-set key persisted for
// kind="dm" rows (sorted agent ids joined with '\n'); "" for other kinds.
// Backs the partial UNIQUE index that makes DM find-or-create race-safe
// across daemons.
func dmMemberKey(kind string, members []GroupDMMember) any {
	if kind != "dm" {
		return nil
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.AgentID
	}
	slices.Sort(ids)
	return strings.Join(ids, "\n")
}

// canonicalKind normalizes empty / unknown kinds to "group" on read so
// legacy rows behave like classic rooms.
func canonicalKind(k string) string {
	if !validGroupDMKinds[k] {
		return "group"
	}
	return k
}

// verifyMembersAlive ensures every id in members exists as a live agent.
// Issued inside the caller's tx so the check and the subsequent insert/
// update are atomic with respect to a concurrent SoftDeleteAgent.
func verifyMembersAlive(ctx context.Context, tx *sql.Tx, members []string) error {
	if len(members) == 0 {
		return nil
	}
	for _, id := range members {
		if id == "" {
			return errors.New("store: empty member id")
		}
		var alive int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, id,
		).Scan(&alive)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: member %q: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// memberIDs projects out just the agent_ids for FK alive-checks.
func memberIDs(in []GroupDMMember) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.AgentID
	}
	return out
}

// addedMemberIDs returns ids that appear in next but not in prev. Used
// by UpdateGroupDM to validate only the diff against the live agents
// table — preserving members that were imported with AllowDeadMembers.
func addedMemberIDs(prev, next []GroupDMMember) []string {
	prevSet := make(map[string]struct{}, len(prev))
	for _, m := range prev {
		prevSet[m.AgentID] = struct{}{}
	}
	var out []string
	for _, m := range next {
		if _, ok := prevSet[m.AgentID]; ok {
			continue
		}
		out = append(out, m.AgentID)
	}
	return out
}

// dedupMembers removes duplicate agent_ids, keeping the *first* occurrence
// (and its NotifyMode/DigestWindow). Reorder is preserved otherwise.
func dedupMembers(in []GroupDMMember) []GroupDMMember {
	seen := make(map[string]bool, len(in))
	out := make([]GroupDMMember, 0, len(in))
	for _, m := range in {
		if seen[m.AgentID] {
			continue
		}
		seen[m.AgentID] = true
		out = append(out, m)
	}
	return out
}

// sameCanonicalMembers returns true when a and b project to the same
// canonical member shape (agent_id + notify_mode + digest_window) ignoring
// declaration order. Used to detect UI re-saves that don't change semantics.
func sameCanonicalMembers(a, b []GroupDMMember) bool {
	ca := canonicalizeMembers(a)
	cb := canonicalizeMembers(b)
	if len(ca) != len(cb) {
		return false
	}
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

// -- groupdm_messages -------------------------------------------------

// GroupDMMessageRecord mirrors the `groupdm_messages` table.
type GroupDMMessageRecord struct {
	ID          string
	GroupDMID   string
	Seq         int64  // per-group
	AgentID     string // "" for system messages (NULL in DB)
	Content     string
	Attachments json.RawMessage
	// Hop is the agent-relay depth: 0 for user/system/fresh agent posts,
	// trigger-hop + 1 for posts made from a notification-triggered turn.
	Hop int
	// Mentions is a JSON array of mentioned member ids ("user" for the
	// human operator). nil when the message mentions nobody.
	Mentions json.RawMessage
	// Usage is a JSON object of token usage for an agent thread reply
	// (mirrors agent.Usage). nil for user/system posts and agent posts
	// made outside a thread turn.
	Usage json.RawMessage

	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

type groupDMMessageETagInput struct {
	ID          string          `json:"id"`
	GroupDMID   string          `json:"groupdm_id"`
	Seq         int64           `json:"seq"`
	AgentID     string          `json:"agent_id"`
	Content     string          `json:"content"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
	Hop         int             `json:"hop,omitempty"`
	Mentions    json.RawMessage `json:"mentions,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
	UpdatedAt   int64           `json:"updated_at"`
	DeletedAt   *int64          `json:"deleted_at"`
}

func computeGroupDMMessageETag(r *GroupDMMessageRecord) (string, error) {
	return CanonicalETag(r.Version, groupDMMessageETagInput{
		ID:          r.ID,
		GroupDMID:   r.GroupDMID,
		Seq:         r.Seq,
		AgentID:     r.AgentID,
		Content:     r.Content,
		Attachments: r.Attachments,
		Hop:         r.Hop,
		Mentions:    r.Mentions,
		Usage:       r.Usage,
		UpdatedAt:   r.UpdatedAt,
		DeletedAt:   r.DeletedAt,
	})
}

// ErrStaleHead is returned by AppendGroupDMMessage when the caller's
// ExpectedLatestSeq does not match the current head — used by the v0
// PostMessage CAS path to surface concurrent-reply conflicts to the UI.
var ErrStaleHead = errors.New("store: stale head (group head advanced)")

// GroupDMMessageInsertOptions matches MessageInsertOptions, plus
// ExpectedLatestSeq for compare-and-set head protection.
type GroupDMMessageInsertOptions struct {
	Now       int64
	CreatedAt int64
	UpdatedAt int64
	Seq       int64
	PeerID    string
	// ExpectedLatestSeq, when non-zero, requires the current per-group
	// head to equal this value — otherwise ErrStaleHead is returned. The
	// API layer uses it to map v0's expectedLatestMessageId conflict
	// flow. ExpectedLatestSeq=0 skips the check (importer/admin path).
	//
	// v0 keys CAS by the head *message id* not seq — the API layer
	// resolves the id via LatestGroupDMMessageID before calling this.
	// We deliberately keep the repo on seq because seq is monotonic and
	// indexed by the UNIQUE(groupdm_id,seq) constraint; the id mapping
	// is a single SELECT on the way in and stays in the API layer.
	ExpectedLatestSeq int64
	// RequireExpectedLatestSeq enforces the head comparison even when
	// ExpectedLatestSeq is 0, making "I expect the room to be EMPTY"
	// expressible: two concurrent first posts to an empty room then
	// collide (the loser gets ErrStaleHead) instead of both passing.
	RequireExpectedLatestSeq bool
	// AllowNonMember lets the caller post under an agent_id that is not
	// in members_json. Reserved for the v0→v1 importer where a member
	// has since been removed but historical messages must round-trip,
	// and for tests. The UserSenderID sentinel is allowed regardless.
	AllowNonMember bool
	// AllowMissingAuthor relaxes the live-author check for the importer:
	// a v0 group transcript may reference an agent_id that has since been
	// hard-deleted, but the message body still needs to round-trip. With
	// this flag set, AppendGroupDMMessage will not query agents and will
	// not enforce membership. Combine with AllowNonMember when both
	// constraints would otherwise block the import. API handlers must
	// leave both flags false.
	AllowMissingAuthor bool
}

// LatestGroupDMMessageID returns the id of the most recent live message in
// groupID, or "" if the group has no messages. Used by the API layer to map
// the client's expectedLatestMessageId into the seq used by
// AppendGroupDMMessage's ExpectedLatestSeq.
func (s *Store) LatestGroupDMMessageID(ctx context.Context, groupID string) (id string, seq int64, err error) {
	const q = `
SELECT m.id, m.seq
  FROM groupdm_messages m
  JOIN groupdms         g ON g.id = m.groupdm_id
 WHERE m.groupdm_id = ? AND m.deleted_at IS NULL AND g.deleted_at IS NULL
 ORDER BY m.seq DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, groupID)
	if err := row.Scan(&id, &seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, nil
		}
		return "", 0, err
	}
	return id, seq, nil
}

// AppendGroupDMMessage inserts a new message at the next per-group seq.
//
// Sender semantics:
//   - rec.AgentID == ""             → system message (NULL in DB)
//   - rec.AgentID == UserSenderID   → human-user post; bypasses
//     member-of-group check, no FK against agents
//   - otherwise → must be a live, member agent unless
//     opts.AllowNonMember is set
func (s *Store) AppendGroupDMMessage(ctx context.Context, rec *GroupDMMessageRecord, opts GroupDMMessageInsertOptions) (*GroupDMMessageRecord, error) {
	if rec == nil {
		return nil, errors.New("store.AppendGroupDMMessage: nil record")
	}
	if rec.ID == "" || rec.GroupDMID == "" {
		return nil, errors.New("store.AppendGroupDMMessage: id/groupdm_id required")
	}

	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}
	created := opts.CreatedAt
	if created == 0 {
		created = now
	}
	updated := opts.UpdatedAt
	if updated == 0 {
		updated = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Parent group DM must be alive — and we need its members_json to
	// validate the author's membership in the same query so the check is
	// consistent with the row we'll write the message against.
	var membersJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT members_json FROM groupdms WHERE id = ? AND deleted_at IS NULL`, rec.GroupDMID,
	).Scan(&membersJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.AppendGroupDMMessage: groupdm %q: %w", rec.GroupDMID, ErrNotFound)
		}
		return nil, err
	}

	// Author validation:
	//   - AgentID == ""              → system, no further check
	//   - AgentID == UserSenderID    → user, no FK / membership check (the
	//                                  user has no row in agents)
	//   - otherwise                  → must be alive and (unless
	//                                  AllowNonMember) a member of this group
	switch {
	case rec.AgentID == "" || rec.AgentID == UserSenderID:
		// System or user — no FK / membership check.
	case opts.AllowMissingAuthor:
		// Importer path: skip both the alive lookup and the membership
		// check. Acceptable because the importer already validated the v0
		// transcript against its own (now-frozen) agents.json.
	default:
		var aOk int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, rec.AgentID,
		).Scan(&aOk)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.AppendGroupDMMessage: author %q: %w", rec.AgentID, ErrNotFound)
		}
		if err != nil {
			return nil, err
		}
		if !opts.AllowNonMember {
			var members []GroupDMMember
			if err := json.Unmarshal([]byte(membersJSON), &members); err != nil {
				return nil, fmt.Errorf("store.AppendGroupDMMessage: decode members: %w", err)
			}
			isMember := false
			for _, m := range members {
				if m.AgentID == rec.AgentID {
					isMember = true
					break
				}
			}
			if !isMember {
				return nil, fmt.Errorf("store.AppendGroupDMMessage: author %q is not a member of %q: %w",
					rec.AgentID, rec.GroupDMID, ErrNotFound)
			}
		}
	}

	// Look up the current head seq first so the ExpectedLatestSeq CAS
	// check and the seq allocation share one snapshot. Without this an
	// extra-lock-free read could see a head value that is already stale
	// by the time the INSERT runs.
	var headSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM groupdm_messages WHERE groupdm_id = ?`, rec.GroupDMID,
	).Scan(&headSeq); err != nil {
		return nil, err
	}
	currentHead := int64(0)
	if headSeq.Valid {
		currentHead = headSeq.Int64
	}
	if (opts.ExpectedLatestSeq != 0 || opts.RequireExpectedLatestSeq) &&
		opts.ExpectedLatestSeq != currentHead {
		return nil, ErrStaleHead
	}
	seq := opts.Seq
	if seq == 0 {
		seq = currentHead + 1
	}

	out := *rec
	out.Seq = seq
	out.Version = 1
	out.CreatedAt = created
	out.UpdatedAt = updated
	out.PeerID = opts.PeerID
	out.DeletedAt = nil
	out.Attachments, err = nullJSON(out.Attachments)
	if err != nil {
		return nil, fmt.Errorf("store.AppendGroupDMMessage: attachments: %w", err)
	}
	out.Mentions, err = nullJSON(out.Mentions)
	if err != nil {
		return nil, fmt.Errorf("store.AppendGroupDMMessage: mentions: %w", err)
	}
	out.Usage, err = nullJSON(out.Usage)
	if err != nil {
		return nil, fmt.Errorf("store.AppendGroupDMMessage: usage: %w", err)
	}
	if out.Hop < 0 {
		out.Hop = 0
	}
	out.ETag, err = computeGroupDMMessageETag(&out)
	if err != nil {
		return nil, err
	}

	const q = `
INSERT INTO groupdm_messages (
  id, groupdm_id, seq, agent_id, content, attachments, hop, mentions, usage,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.GroupDMID, out.Seq, nullableText(out.AgentID),
		nullableText(out.Content), nullableRaw(out.Attachments),
		out.Hop, nullableRaw(out.Mentions), nullableRaw(out.Usage),
		out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
	); err != nil {
		return nil, fmt.Errorf("store.AppendGroupDMMessage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
}

// GroupDMMessageListOptions matches MessageListOptions.
type GroupDMMessageListOptions struct {
	Limit     int
	BeforeSeq int64
	SinceSeq  int64
	Order     string // "asc"|"desc"
}

// ListGroupDMMessages returns messages for groupID. Hides messages of
// tombstoned parent groups (cascade-on-tombstone via JOIN).
func (s *Store) ListGroupDMMessages(ctx context.Context, groupID string, opts GroupDMMessageListOptions) ([]*GroupDMMessageRecord, error) {
	if groupID == "" {
		return nil, errors.New("store.ListGroupDMMessages: groupdm_id required")
	}
	args := []any{groupID}
	q := `
SELECT m.id, m.groupdm_id, m.seq, COALESCE(m.agent_id,''),
       COALESCE(m.content,''), m.attachments, m.hop, m.mentions, m.usage,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM groupdm_messages m
  JOIN groupdms         g ON g.id = m.groupdm_id
 WHERE m.groupdm_id = ? AND m.deleted_at IS NULL AND g.deleted_at IS NULL`
	if opts.BeforeSeq > 0 {
		q += ` AND m.seq < ?`
		args = append(args, opts.BeforeSeq)
	}
	if opts.SinceSeq > 0 {
		q += ` AND m.seq > ?`
		args = append(args, opts.SinceSeq)
	}
	order := "ASC"
	if opts.Order == "desc" {
		order = "DESC"
	}
	q += ` ORDER BY m.seq ` + order
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GroupDMMessageRecord
	for rows.Next() {
		rec, err := scanGroupDMMessageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SoftDeleteGroupDMMessages tombstones every live message in groupID and
// returns the number of rows newly marked deleted. The parent group must be
// live; deleting a transcript for an unknown or tombstoned group is treated
// as ErrNotFound so callers don't silently clear stale views.
func (s *Store) SoftDeleteGroupDMMessages(ctx context.Context, groupID string) (int64, error) {
	if groupID == "" {
		return 0, errors.New("store.SoftDeleteGroupDMMessages: groupdm_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM groupdms WHERE id = ? AND deleted_at IS NULL`, groupID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}

	now := NowMillis()
	const q = `
UPDATE groupdm_messages
   SET deleted_at = ?, updated_at = ?, version = version + 1
 WHERE groupdm_id = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, q, now, now, groupID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func scanGroupDMMessageRow(r rowScanner) (*GroupDMMessageRecord, error) {
	var (
		rec         GroupDMMessageRecord
		attachments sql.NullString
		mentions    sql.NullString
		usage       sql.NullString
		deletedAt   sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.GroupDMID, &rec.Seq, &rec.AgentID,
		&rec.Content, &attachments, &rec.Hop, &mentions, &usage,
		&rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if attachments.Valid {
		rec.Attachments = json.RawMessage(attachments.String)
	}
	if mentions.Valid {
		rec.Mentions = json.RawMessage(mentions.String)
	}
	if usage.Valid {
		rec.Usage = json.RawMessage(usage.String)
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// -- groupdm_dead_letters ----------------------------------------------

// GroupDMDeadLetter is a permanently failed notification delivery. Kept
// as an audit record instead of silently dropping the batch. Payload is
// the rendered notification text (may be truncated by the caller).
type GroupDMDeadLetter struct {
	ID        int64  `json:"id"`
	GroupDMID string `json:"groupdmId"`
	AgentID   string `json:"agentId"`
	Reason    string `json:"reason"`
	Payload   string `json:"payload,omitempty"`
	Attempts  int    `json:"attempts"`
	CreatedAt int64  `json:"createdAt"`
}

// InsertGroupDMDeadLetter records a permanently failed delivery.
func (s *Store) InsertGroupDMDeadLetter(ctx context.Context, dl *GroupDMDeadLetter) error {
	if dl == nil || dl.GroupDMID == "" || dl.AgentID == "" {
		return errors.New("store.InsertGroupDMDeadLetter: groupdm_id/agent_id required")
	}
	if dl.CreatedAt == 0 {
		dl.CreatedAt = NowMillis()
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO groupdm_dead_letters (groupdm_id, agent_id, reason, payload, attempts, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		dl.GroupDMID, dl.AgentID, dl.Reason, dl.Payload, dl.Attempts, dl.CreatedAt)
	if err != nil {
		return fmt.Errorf("store.InsertGroupDMDeadLetter: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		dl.ID = id
	}
	return nil
}

// ListGroupDMDeadLetters returns dead letters for a group, newest first,
// capped at limit (0 = 50).
func (s *Store) ListGroupDMDeadLetters(ctx context.Context, groupID string, limit int) ([]*GroupDMDeadLetter, error) {
	if groupID == "" {
		return nil, errors.New("store.ListGroupDMDeadLetters: groupdm_id required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, groupdm_id, agent_id, reason, COALESCE(payload,''), attempts, created_at
  FROM groupdm_dead_letters
 WHERE groupdm_id = ?
 ORDER BY created_at DESC, id DESC
 LIMIT ?`, groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GroupDMDeadLetter
	for rows.Next() {
		var dl GroupDMDeadLetter
		if err := rows.Scan(&dl.ID, &dl.GroupDMID, &dl.AgentID, &dl.Reason, &dl.Payload, &dl.Attempts, &dl.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &dl)
	}
	return out, rows.Err()
}

// SetGroupDMReadCursor persists the operator's read cursor (highest read
// seq) for a room. Idempotent upsert; a lower seq never overwrites a higher
// one so an out-of-order/stale mark-read can't resurrect unread badges.
func (s *Store) SetGroupDMReadCursor(ctx context.Context, groupID string, seq int64) error {
	if groupID == "" {
		return errors.New("store.SetGroupDMReadCursor: groupdm_id required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO groupdm_read_cursors (groupdm_id, last_read_seq, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(groupdm_id) DO UPDATE SET
  last_read_seq = MAX(last_read_seq, excluded.last_read_seq),
  updated_at    = excluded.updated_at`,
		groupID, seq, NowMillis())
	return err
}

// GetGroupDMReadCursor returns the persisted read-cursor seq for a room.
// ok is false when no cursor has been recorded yet (the room has never been
// marked read on any device).
func (s *Store) GetGroupDMReadCursor(ctx context.Context, groupID string) (seq int64, ok bool, err error) {
	if groupID == "" {
		return 0, false, errors.New("store.GetGroupDMReadCursor: groupdm_id required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT last_read_seq FROM groupdm_read_cursors WHERE groupdm_id = ?`, groupID)
	err = row.Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return seq, true, nil
}
