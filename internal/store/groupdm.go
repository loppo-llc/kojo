package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

type groupDMETagInput struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	MembersSorted []canonicalMember `json:"members_sorted"`
	Style         string            `json:"style"`
	Cooldown      int               `json:"cooldown"`
	Venue         string            `json:"venue"`
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
  id, name, members_json, style, cooldown, venue,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.Name, string(membersJSON), out.Style, out.Cooldown, out.Venue,
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
SELECT id, name, members_json, style, cooldown, venue, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
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
SELECT id, name, members_json, style, cooldown, venue, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
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
SELECT id, name, members_json, style, cooldown, venue, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
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
  name = ?, members_json = ?, style = ?, cooldown = ?, venue = ?,
  version = ?, etag = ?, updated_at = ?
WHERE id = ? AND etag = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, upd,
		next.Name, string(membersJSON), next.Style, next.Cooldown, next.Venue,
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

// SoftDeleteGroupDM tombstones the group DM. Idempotent. Recomputes etag.
func (s *Store) SoftDeleteGroupDM(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT id, name, members_json, style, cooldown, venue, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
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
		&rec.ID, &rec.Name, &members, &rec.Style, &rec.Cooldown, &rec.Venue,
		&rec.Seq, &rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
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
	Seq         int64 // per-group
	AgentID     string // "" for system messages (NULL in DB)
	Content     string
	Attachments json.RawMessage

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
	if opts.ExpectedLatestSeq != 0 && opts.ExpectedLatestSeq != currentHead {
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
	out.ETag, err = computeGroupDMMessageETag(&out)
	if err != nil {
		return nil, err
	}

	const q = `
INSERT INTO groupdm_messages (
  id, groupdm_id, seq, agent_id, content, attachments,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.GroupDMID, out.Seq, nullableText(out.AgentID),
		nullableText(out.Content), nullableRaw(out.Attachments),
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
       COALESCE(m.content,''), m.attachments,
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

func scanGroupDMMessageRow(r rowScanner) (*GroupDMMessageRecord, error) {
	var (
		rec         GroupDMMessageRecord
		attachments sql.NullString
		deletedAt   sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.GroupDMID, &rec.Seq, &rec.AgentID,
		&rec.Content, &attachments,
		&rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if attachments.Valid {
		rec.Attachments = json.RawMessage(attachments.String)
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}
