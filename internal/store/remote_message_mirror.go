package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Hub 側で §3.7 device-switch により他 peer に転移している agent の
// messages を一時キャッシュする mirror テーブルへのアクセサ。
//
// canonical な agent_messages とは独立しており、oplog 経路にも参加しない。
// proxyPeerGetMessages / fetchRemoteLatestMessage の成功時に upsert され、
// peer 帰還 (force-reclaim / arrival で holder==self) と ReleaseAgentLocally
// で agent_id 単位に delete される。
//
// 詳細は migrations/0019_remote_message_mirror.sql のヘッダコメントを参照。

// RemoteMirrorMessage は mirror テーブル 1 行の wire 表現。
// agent.Message と field 名・型を揃えており、handlers 層で agent.Message
// にそのままコピーできる。
type RemoteMirrorMessage struct {
	ID          string
	Role        string
	Content     string
	Thinking    string
	ToolUses    []byte // JSON, opaque (nil 可)
	Attachments []byte // JSON, opaque (nil 可)
	Usage       []byte // JSON, opaque (nil 可)
	Timestamp   string // RFC3339
}

const remoteMirrorUpsertSQL = `
INSERT INTO remote_message_mirror (
  agent_id, message_id, role, content, thinking,
  tool_uses, attachments, usage,
  timestamp, holder_peer, mirrored_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_id, message_id) DO UPDATE SET
  role        = excluded.role,
  content     = excluded.content,
  thinking    = excluded.thinking,
  tool_uses   = excluded.tool_uses,
  attachments = excluded.attachments,
  usage       = excluded.usage,
  timestamp   = excluded.timestamp,
  holder_peer = excluded.holder_peer,
  mirrored_at = excluded.mirrored_at`

// UpsertRemoteMirrorMessages は msgs を agent_id 単位に upsert する。
// message_id PK で dedup される。空 slice は no-op。1 トランザクション。
func (s *Store) UpsertRemoteMirrorMessages(ctx context.Context, agentID, holderPeer string, msgs []RemoteMirrorMessage) error {
	return s.upsertRemoteMirrorMessages(ctx, agentID, holderPeer, "", msgs)
}

// UpsertRemoteMirrorMessagesIfHolder は agent_locks.holder_peer が
// expectedHolder と一致するときだけ upsert する。in-flight proxy 応答が
// finalize/force-reclaim 直後に到着して stale 行を再挿入するレースを
// 単一 BEGIN IMMEDIATE トランザクション内で閉じる。holder が一致しない
// (または lock 行が消えている) ときは upsert を skip し、エラーは返さない。
func (s *Store) UpsertRemoteMirrorMessagesIfHolder(ctx context.Context, agentID, expectedHolder string, msgs []RemoteMirrorMessage) error {
	if expectedHolder == "" {
		return errors.New("store.UpsertRemoteMirrorMessagesIfHolder: expectedHolder required")
	}
	return s.upsertRemoteMirrorMessages(ctx, agentID, expectedHolder, expectedHolder, msgs)
}

// upsertRemoteMirrorMessages は内部実装。expectedHolder != "" のとき
// agent_locks.holder_peer == expectedHolder を tx 内で確認し、不一致なら
// upsert を skip する。expectedHolder == "" のときは無条件 upsert。
func (s *Store) upsertRemoteMirrorMessages(ctx context.Context, agentID, holderPeer, expectedHolder string, msgs []RemoteMirrorMessage) error {
	if s == nil || s.db == nil {
		return errors.New("store.UpsertRemoteMirrorMessages: store not initialised")
	}
	if agentID == "" {
		return errors.New("store.UpsertRemoteMirrorMessages: agent_id required")
	}
	if holderPeer == "" {
		return errors.New("store.UpsertRemoteMirrorMessages: holder_peer required")
	}
	if len(msgs) == 0 {
		return nil
	}
	// BEGIN IMMEDIATE: holder check + upsert を同一 tx で直列化。これに
	// より「check 通過 → 別 tx で DeleteMirrorForAgent → upsert で復活」の
	// race window を閉じる。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.UpsertRemoteMirrorMessages: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if expectedHolder != "" {
		var curHolder string
		err := tx.QueryRowContext(ctx,
			`SELECT holder_peer FROM agent_locks WHERE agent_id = ?`, agentID).Scan(&curHolder)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// agent_locks 行が消えている = この peer (Hub) が holder に
			// 戻った直後 (AcquireAgentLock 前) か、agent 自体が削除済み。
			// どちらも upsert すべきでない (stale 化する) ので skip。
			return nil
		case err != nil:
			return fmt.Errorf("store.UpsertRemoteMirrorMessages: holder check: %w", err)
		}
		if curHolder != expectedHolder {
			// holder rotated — proxy 先 peer は既に元 holder ではない。
			// その応答を mirror に入れると stale。skip。
			return nil
		}
	}

	now := NowMillis()
	stmt, err := tx.PrepareContext(ctx, remoteMirrorUpsertSQL)
	if err != nil {
		return fmt.Errorf("store.UpsertRemoteMirrorMessages: prepare: %w", err)
	}
	defer stmt.Close()

	for i, m := range msgs {
		if m.ID == "" {
			return fmt.Errorf("store.UpsertRemoteMirrorMessages: msg[%d] id required", i)
		}
		if m.Role == "" {
			return fmt.Errorf("store.UpsertRemoteMirrorMessages: msg[%d] role required", i)
		}
		if m.Timestamp == "" {
			return fmt.Errorf("store.UpsertRemoteMirrorMessages: msg[%d] timestamp required", i)
		}
		if _, err := stmt.ExecContext(ctx,
			agentID, m.ID, m.Role, m.Content, m.Thinking,
			jsonOrNil(m.ToolUses), jsonOrNil(m.Attachments), jsonOrNil(m.Usage),
			m.Timestamp, holderPeer, now,
		); err != nil {
			return fmt.Errorf("store.UpsertRemoteMirrorMessages: exec msg[%d] %q: %w", i, m.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.UpsertRemoteMirrorMessages: commit: %w", err)
	}
	return nil
}

// ListRemoteMirrorMessages は agent の mirror から timestamp DESC 順で
// 最新 limit 件 (oldest-first で返却) を取得する。before に message_id を
// 渡すと、その id より古い (timestamp が小さい、同 ts なら message_id が小さい)
// 行を limit 件返す。MessagesPaginated と同じ envelope (oldest-first slice +
// hasMore) を提供する。limit <= 0 は全件返却 + hasMore=false。
func (s *Store) ListRemoteMirrorMessages(ctx context.Context, agentID string, limit int, before string) ([]RemoteMirrorMessage, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("store.ListRemoteMirrorMessages: store not initialised")
	}
	if agentID == "" {
		return nil, false, errors.New("store.ListRemoteMirrorMessages: agent_id required")
	}

	// before cursor: ts + message_id を引いてから WHERE (ts,id) < (refTs,refID)
	// で paginate する。canonical 側 (loadMessagesPaginated) と同じく
	// 「不明な before は『何の前でもない』として無視」する。
	var (
		refTS string
		refID string
	)
	if before != "" {
		row := s.db.QueryRowContext(ctx,
			`SELECT timestamp, message_id FROM remote_message_mirror
			  WHERE agent_id = ? AND message_id = ?`, agentID, before)
		if err := row.Scan(&refTS, &refID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("store.ListRemoteMirrorMessages: resolve cursor: %w", err)
		}
	}

	// limit+1 を読んで hasMore 判定。limit <= 0 は全件。
	args := []any{agentID}
	q := `SELECT message_id, role, content, thinking, tool_uses, attachments, usage, timestamp
	        FROM remote_message_mirror
	       WHERE agent_id = ?`
	if refTS != "" {
		q += ` AND (timestamp < ? OR (timestamp = ? AND message_id < ?))`
		args = append(args, refTS, refTS, refID)
	}
	q += ` ORDER BY timestamp DESC, message_id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit+1)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store.ListRemoteMirrorMessages: query: %w", err)
	}
	defer rows.Close()

	out := make([]RemoteMirrorMessage, 0)
	for rows.Next() {
		var m RemoteMirrorMessage
		var toolUses, attachments, usage sql.NullString
		if err := rows.Scan(
			&m.ID, &m.Role, &m.Content, &m.Thinking,
			&toolUses, &attachments, &usage,
			&m.Timestamp,
		); err != nil {
			return nil, false, fmt.Errorf("store.ListRemoteMirrorMessages: scan: %w", err)
		}
		if toolUses.Valid {
			m.ToolUses = []byte(toolUses.String)
		}
		if attachments.Valid {
			m.Attachments = []byte(attachments.String)
		}
		if usage.Valid {
			m.Usage = []byte(usage.String)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store.ListRemoteMirrorMessages: rows: %w", err)
	}

	hasMore := false
	if limit > 0 && len(out) > limit {
		hasMore = true
		out = out[:limit]
	}
	// DESC で取った結果を oldest-first に反転
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, hasMore, nil
}

// RemoteMirrorMessageExists は (agent_id, message_id) の行が存在するか
// だけを判定する。serveRemoteMessagesMerged が cursor の出所 source を
// 判別するために使う。エラーは false + err で返す。
func (s *Store) RemoteMirrorMessageExists(ctx context.Context, agentID, messageID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("store.RemoteMirrorMessageExists: store not initialised")
	}
	if agentID == "" || messageID == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM remote_message_mirror
		  WHERE agent_id = ? AND message_id = ? LIMIT 1`,
		agentID, messageID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store.RemoteMirrorMessageExists: %w", err)
	}
	return true, nil
}

// DeleteRemoteMirrorForAgent は agent の mirror 行を全て削除する。
// 帰還時 (holder==self になった瞬間) と ReleaseAgentLocally で呼ぶ。
// 戻り値は削除された行数。
func (s *Store) DeleteRemoteMirrorForAgent(ctx context.Context, agentID string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store.DeleteRemoteMirrorForAgent: store not initialised")
	}
	if agentID == "" {
		return 0, errors.New("store.DeleteRemoteMirrorForAgent: agent_id required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM remote_message_mirror WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, fmt.Errorf("store.DeleteRemoteMirrorForAgent: exec: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReplaceRemoteMirrorWindowIfHolder replaces the mirrored window for
// one agent with the holder's authoritative latest-N snapshot, inside
// the same holder-checked BEGIN IMMEDIATE transaction shape as
// UpsertRemoteMirrorMessagesIfHolder.
//
// Semantics: msgs is the holder's newest window (a GET ?limit=N with
// no `before` cursor). Any mirror row that sorts inside that window —
// (timestamp, message_id) >= the oldest fetched message — but was NOT
// returned by the holder has been deleted there, so it is pruned here.
// Rows older than the window are preserved (they came from deeper
// paginated proxy reads and the holder said nothing about them).
// msgs == empty means the holder's transcript is empty: the whole
// mirror for the agent is cleared. Callers must only pass empty msgs
// when the holder reported hasMore=false.
//
// The holder check (agent_locks.holder_peer == expectedHolder) skips
// the entire operation — including the prune — when the agent moved
// or came home, so a stale in-flight response can't clear a mirror it
// no longer owns.
func (s *Store) ReplaceRemoteMirrorWindowIfHolder(ctx context.Context, agentID, expectedHolder string, msgs []RemoteMirrorMessage) error {
	if s == nil || s.db == nil {
		return errors.New("store.ReplaceRemoteMirrorWindowIfHolder: store not initialised")
	}
	if agentID == "" {
		return errors.New("store.ReplaceRemoteMirrorWindowIfHolder: agent_id required")
	}
	if expectedHolder == "" {
		return errors.New("store.ReplaceRemoteMirrorWindowIfHolder: expectedHolder required")
	}
	for i, m := range msgs {
		if m.ID == "" || m.Role == "" || m.Timestamp == "" {
			return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: msg[%d] id/role/timestamp required", i)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Same holder gate as upsertRemoteMirrorMessages: no lock row or a
	// rotated holder means this response is stale — skip silently.
	var curHolder string
	switch err := tx.QueryRowContext(ctx,
		`SELECT holder_peer FROM agent_locks WHERE agent_id = ?`, agentID).Scan(&curHolder); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: holder check: %w", err)
	}
	if curHolder != expectedHolder {
		return nil
	}

	if len(msgs) == 0 {
		// Holder transcript is empty and complete — clear the mirror.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM remote_message_mirror WHERE agent_id = ?`, agentID); err != nil {
			return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: clear: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: commit: %w", err)
		}
		return nil
	}

	// Window lower bound = oldest fetched message by the mirror's own
	// sort order (timestamp, message_id). RFC3339 timestamps compare
	// correctly as strings, matching the SQL used by list/paginate.
	minTS, minID := msgs[0].Timestamp, msgs[0].ID
	for _, m := range msgs[1:] {
		if m.Timestamp < minTS || (m.Timestamp == minTS && m.ID < minID) {
			minTS, minID = m.Timestamp, m.ID
		}
	}

	// Prune rows inside the window that the holder no longer returned.
	delQ := `DELETE FROM remote_message_mirror
	          WHERE agent_id = ?
	            AND (timestamp > ? OR (timestamp = ? AND message_id >= ?))
	            AND message_id NOT IN (`
	delArgs := []any{agentID, minTS, minTS, minID}
	for i, m := range msgs {
		if i > 0 {
			delQ += ","
		}
		delQ += "?"
		delArgs = append(delArgs, m.ID)
	}
	delQ += ")"
	if _, err := tx.ExecContext(ctx, delQ, delArgs...); err != nil {
		return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: prune: %w", err)
	}

	now := NowMillis()
	stmt, err := tx.PrepareContext(ctx, remoteMirrorUpsertSQL)
	if err != nil {
		return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: prepare: %w", err)
	}
	defer stmt.Close()
	for i, m := range msgs {
		if _, err := stmt.ExecContext(ctx,
			agentID, m.ID, m.Role, m.Content, m.Thinking,
			jsonOrNil(m.ToolUses), jsonOrNil(m.Attachments), jsonOrNil(m.Usage),
			m.Timestamp, expectedHolder, now,
		); err != nil {
			return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: exec msg[%d] %q: %w", i, m.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.ReplaceRemoteMirrorWindowIfHolder: commit: %w", err)
	}
	return nil
}
