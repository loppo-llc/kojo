package importers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
)

// TestOpenV0RefusesLeafSymlink exercises the leaf Lstat guard: a
// symlink at the leaf (regardless of target) is refused with
// ErrNotRegular before the prefix check even runs. Lstat-first ensures
// a broken or hostile symlink doesn't surface as ErrNotExist (which
// callers treat as "missing optional file").
func TestOpenV0RefusesLeafSymlink(t *testing.T) {
	v0 := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(target, []byte("not-for-import"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(v0, "agents", "ag_x", "persona.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	if _, err := readV0(v0, link); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("expected ErrNotRegular, got %v", err)
	}
}

// TestOpenV0RefusesParentSymlinkEscape covers the dir-component
// escape: leaf is a regular file, but a parent dir is a symlink that
// resolves outside V0Dir. EvalSymlinks-based prefix check catches it
// with ErrEscapesRoot.
func TestOpenV0RefusesParentSymlinkEscape(t *testing.T) {
	v0 := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "ag_x"), 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	target := filepath.Join(outside, "ag_x", "persona.md")
	if err := os.WriteFile(target, []byte("not-for-import"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(v0, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	parentLink := filepath.Join(v0, "agents", "ag_x")
	if err := os.Symlink(filepath.Join(outside, "ag_x"), parentLink); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	leaf := filepath.Join(parentLink, "persona.md")

	if _, err := readV0(v0, leaf); !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("expected ErrEscapesRoot, got %v", err)
	}
}

// TestImporterOrder pins the registration sequence to the design's
// dependency graph. agents must come first because messages and
// groupdms FK against agent_id; reshuffling without updating the
// schema or the importers would silently break first-time migration.
func TestImporterOrder(t *testing.T) {
	got := []string{}
	for _, imp := range importerOrder() {
		got = append(got, imp.Domain())
	}
	want := []string{"agents", "messages", "groupdms", "tasks", "sessions", "notify_cursors", "vapid", "push_subscriptions", "external_chat_cursors", "compactions", "blobs"}
	if len(got) != len(want) {
		t.Fatalf("importerOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("importerOrder[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestImportersRoundTrip wires up a fake v0 directory tree, runs every
// registered importer in order, and asserts that the round-trip ends up
// with the records the v0 fixtures describe. The fixtures are minimal —
// just enough to exercise each importer's branches.
func TestImportersRoundTrip(t *testing.T) {
	v0 := t.TempDir()
	v1 := t.TempDir()
	writeV0Fixtures(t, v0)

	st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}

	// Run every importer in registration order. importerOrder() is the
	// single source of truth — using it here is what makes the
	// "registration order matches design contract" assertion below
	// meaningful (otherwise the test would happily pass even if an
	// importer was reshuffled out of the agents-first invariant).
	for _, imp := range importerOrder() {
		if err := imp.Run(ctx, st, opts); err != nil {
			t.Fatalf("importer %s: %v", imp.Domain(), err)
		}
	}

	// agents
	agents, err := st.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	if agents[0].ID != "ag_1" || agents[0].Name != "Alice" {
		t.Errorf("agent[0] = %+v", agents[0])
	}
	if v, ok := agents[0].Settings["model"].(string); !ok || v != "sonnet" {
		t.Errorf("agent[0].settings.model = %v", agents[0].Settings["model"])
	}

	// agent_persona — body comes from persona.md (overrides inline persona)
	p, err := st.GetAgentPersona(ctx, "ag_1")
	if err != nil {
		t.Fatalf("persona ag_1: %v", err)
	}
	if p.Body != "alice persona body\n" {
		t.Errorf("persona body = %q", p.Body)
	}

	// ag_2 has no persona.md, only inline — must come from agents.json
	p2, err := st.GetAgentPersona(ctx, "ag_2")
	if err != nil {
		t.Fatalf("persona ag_2: %v", err)
	}
	if p2.Body != "inline only" {
		t.Errorf("persona ag_2 body = %q", p2.Body)
	}

	// agent_memory
	mem, err := st.GetAgentMemory(ctx, "ag_1")
	if err != nil {
		t.Fatalf("memory ag_1: %v", err)
	}
	if mem.Body != "MEMORY index for alice\n" {
		t.Errorf("memory body = %q", mem.Body)
	}

	// memory_entries — daily, project, topic, people, archive coverage
	mes, err := st.ListMemoryEntries(ctx, "ag_1", store.MemoryEntryListOptions{})
	if err != nil {
		t.Fatalf("list memory: %v", err)
	}
	got := map[string]string{} // kind+name → body
	for _, e := range mes {
		got[e.Kind+"|"+e.Name] = e.Body
	}
	want := map[string]string{
		"daily|2026-04-01":      "daily 1\n",
		"daily|2026-04-02":      "daily 2\n",
		"project|kojo":          "project kojo\n",
		"people|akari":          "people akari\n",
		"topic|release":         "topic release\n",
		"archive|2026-03":       "archived march\n",
		"topic|loose":           "loose topic file\n", // top-level non-date
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("memory_entries[%s] = %q, want %q", k, got[k], v)
		}
	}

	// agent_messages — appends preserve order via per-agent seq.
	msgs, err := st.ListMessages(ctx, "ag_1", store.MessageListOptions{})
	if err != nil {
		t.Fatalf("list msgs: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("msgs = %d, want 3", len(msgs))
	}
	if msgs[0].ID != "m_a" || msgs[0].Content != "hi" {
		t.Errorf("msg[0] = %+v", msgs[0])
	}
	if msgs[2].Role != "assistant" {
		t.Errorf("msg[2].role = %q", msgs[2].Role)
	}

	// groupdms + members
	groups, err := st.ListGroupDMs(ctx)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if groups[0].ID != "gd_1" || groups[0].Style != "efficient" {
		t.Errorf("group = %+v", groups[0])
	}
	if len(groups[0].Members) != 2 {
		t.Errorf("members = %d, want 2", len(groups[0].Members))
	}

	// blobs — every leaf written by writeV0Fixtures must have a
	// blob_refs row addressed under the right scope, with size and
	// sha256 matching the on-disk body. The home_peer column reflects
	// opts.HomePeer (deterministic across hosts).
	type blobCase struct {
		uri, scope, body string
	}
	cases := []blobCase{
		{"kojo://global/agents/ag_1/avatar.png", "global", "\x89PNG"},
		{"kojo://global/agents/ag_1/books/intro.md", "global", "intro book\n"},
		{"kojo://global/agents/ag_1/books/ch01.md", "global", "chapter one\n"},
		{"kojo://global/agents/ag_1/outbox/draft.txt", "global", "draft body\n"},
		{"kojo://local/agents/ag_1/temp/scratch.json", "local", `{"k":"v"}`},
		{"kojo://local/agents/ag_1/index/memory.db", "local", "SQLITE-FAKE-BLOB"},
		{"kojo://machine/agents/ag_1/credentials.json", "machine", `{"token":"x"}`},
		{"kojo://machine/agents/ag_1/credentials.key", "machine", "ENVELOPE-KEY"},
		// Phase C catchall walk: top-level scratch + outputs/ +
		// arbitrary subdirs land at scope=local.
		{"kojo://local/agents/ag_1/game-design-doc.md", "local", "scratch design doc\n"},
		{"kojo://local/agents/ag_1/outputs/result.bvh", "local", "BVH-FAKE\n"},
		{"kojo://local/agents/ag_1/outputs/timing.json", "local", `{"t":1}`},
		{"kojo://local/agents/ag_1/projects/kojo-v1/notes.md", "local", "project notes\n"},
		{"kojo://local/agents/ag_1/research/x-cookies/session.json", "local", `{"sid":"abc"}`},
	}
	for _, c := range cases {
		ref, err := st.GetBlobRef(ctx, c.uri)
		if err != nil {
			t.Errorf("blob_refs[%s]: %v", c.uri, err)
			continue
		}
		if ref.Scope != c.scope {
			t.Errorf("blob_refs[%s].scope = %q, want %q", c.uri, ref.Scope, c.scope)
		}
		if ref.Size != int64(len(c.body)) {
			t.Errorf("blob_refs[%s].size = %d, want %d", c.uri, ref.Size, len(c.body))
		}
		if ref.HomePeer != "peer-test" {
			t.Errorf("blob_refs[%s].home_peer = %q, want peer-test", c.uri, ref.HomePeer)
		}
		if ref.SHA256 == "" || len(ref.SHA256) != 64 {
			t.Errorf("blob_refs[%s].sha256 = %q (len %d)", c.uri, ref.SHA256, len(ref.SHA256))
		}
	}

	// Suppression assertions: each URI MUST NOT have a blob_refs row.
	// These cover the skipBlobDir / skipBlobFile / canonicalDBLeaves
	// branches independently. ErrNotFound is the contract; any other
	// outcome indicates a regression in the suppression rules.
	suppressed := []string{
		// canonicalDBLeaves — DB-canonical, no blob duplicate.
		"kojo://global/agents/ag_1/persona.md",
		"kojo://local/agents/ag_1/persona.md",
		"kojo://global/agents/ag_1/MEMORY.md",
		"kojo://local/agents/ag_1/MEMORY.md",
		"kojo://local/agents/ag_1/messages.jsonl",
		"kojo://local/agents/ag_1/tasks.json",
		"kojo://local/agents/ag_1/persona_summary.md",
		// memory/ DB-canonical entries — runtime hydrate from DB.
		"kojo://local/agents/ag_1/memory/2026-04-01.md",
		"kojo://local/agents/ag_1/memory/projects/kojo.md",
		"kojo://local/agents/ag_1/memory/people/akari.md",
		// chat_history body skipped (re-fetched from platform).
		"kojo://local/agents/ag_1/chat_history/slack/C123/_channel.jsonl",
		// blobBackupRe / dotfile / .lock skips.
		"kojo://local/agents/ag_1/MEMORY.md.bak",
		"kojo://local/agents/ag_1/messages.jsonl.bak2",
		"kojo://local/agents/ag_1/.DS_Store",
		"kojo://local/agents/ag_1/kojo.lock",
		// CLI workspace dotdirs.
		"kojo://local/agents/ag_1/.claude/settings.local.json",
		"kojo://local/agents/ag_1/.codex/session.json",
		"kojo://local/agents/ag_1/.gemini/settings.json",
	}
	for _, u := range suppressed {
		if _, err := st.GetBlobRef(ctx, u); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("blob_refs[%s] should be suppressed; got err=%v", u, err)
		}
	}

	// sessions — every v0 row lands as 'archived' regardless of its
	// v0 status (PTY died with v0). The id-less row is skipped so the
	// imported count is exactly 2.
	if got, err := st.GetSession(ctx, "sess_alive"); err != nil {
		t.Fatalf("get sess_alive: %v", err)
	} else {
		if got.Status != "archived" {
			t.Errorf("sess_alive status = %q, want archived (v0 'running' must be demoted)", got.Status)
		}
		if got.PeerID != "peer-test" {
			t.Errorf("sess_alive peer_id = %q, want peer-test", got.PeerID)
		}
		if got.Cmd != "claude --dev" {
			t.Errorf("sess_alive cmd = %q, want 'claude --dev'", got.Cmd)
		}
	}
	if got, err := st.GetSession(ctx, "sess_done"); err != nil {
		t.Fatalf("get sess_done: %v", err)
	} else if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("sess_done exit_code = %v, want &0", got.ExitCode)
	}

	// agent_tasks — open → pending, done → done, in_progress passes
	// through. Seq matches input order (1-based).
	tlist, err := st.ListAgentTasks(ctx, "ag_1", store.AgentTaskListOptions{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tlist) != 3 {
		t.Fatalf("tasks = %d, want 3", len(tlist))
	}
	wantTasks := []struct {
		id, title, status string
		seq               int64
	}{
		{"task_a", "ship release", "pending", 1},
		{"task_b", "write docs", "done", 2},
		{"task_c", "draft notes", "in_progress", 3},
	}
	for i, w := range wantTasks {
		if tlist[i].ID != w.id || tlist[i].Title != w.title || tlist[i].Status != w.status || tlist[i].Seq != w.seq {
			t.Errorf("task[%d] = %+v, want id=%q title=%q status=%q seq=%d",
				i, tlist[i], w.id, w.title, w.status, w.seq)
		}
	}

	// notify_cursors — resolvable cursors get a v1 composite id with
	// type segment; orphans (source not declared in agents.json) and
	// empty cursors are skipped. Imported rows are listed under the
	// owning agent in id-asc order.
	ncs, err := st.ListNotifyCursorsByAgent(ctx, "ag_1")
	if err != nil {
		t.Fatalf("list notify_cursors ag_1: %v", err)
	}
	gotNC := map[string]string{}
	for _, r := range ncs {
		gotNC[r.ID] = r.Source + "|" + r.Cursor
	}
	wantNC := map[string]string{
		"ag_1:gmail:src_gmail": "gmail|gmail-cursor-1",
		"ag_1:slack:src_slack": "slack|slack-cursor-1",
	}
	if len(gotNC) != len(wantNC) {
		t.Errorf("notify_cursors = %v, want %v", gotNC, wantNC)
	}
	for k, v := range wantNC {
		if gotNC[k] != v {
			t.Errorf("notify_cursors[%s] = %q, want %q", k, gotNC[k], v)
		}
	}
	// Orphan / empty-cursor rows must NOT appear under any composite
	// id — confirms the warn-skip rather than silent miscategorization.
	for _, want := range []string{
		"ag_1:orphan_src", "ag_1:src_empty",
		"ag_1::orphan_src", "ag_1::src_empty",
	} {
		if _, err := st.GetNotifyCursor(ctx, want); err == nil {
			t.Errorf("orphan / empty cursor leaked into store: %q", want)
		}
	}

	// push_subscriptions — well-formed rows are imported with
	// vapid_public_key copied from vapid.json; malformed rows (missing
	// endpoint or auth) are dropped via the warn-skip path. ListActive
	// returns rows where expired_at IS NULL — every imported row stays
	// active because v0 dropped expired rows from the file rather than
	// tombstoning them.
	psubs, err := st.ListActivePushSubscriptions(ctx)
	if err != nil {
		t.Fatalf("list push_subscriptions: %v", err)
	}
	gotPS := map[string]string{}
	for _, r := range psubs {
		gotPS[r.Endpoint] = r.VAPIDPublicKey + "|" + r.P256dh + "|" + r.Auth
	}
	wantPS := map[string]string{
		"https://push.example.com/ep_a": "vapid-pub-fixture|p256-a|auth-a",
		"https://push.example.com/ep_b": "vapid-pub-fixture|p256-b|auth-b",
	}
	if len(gotPS) != len(wantPS) {
		t.Errorf("push_subscriptions = %v, want %v", gotPS, wantPS)
	}
	for k, v := range wantPS {
		if gotPS[k] != v {
			t.Errorf("push_subscriptions[%s] = %q, want %q", k, gotPS[k], v)
		}
	}
	// Malformed rows must NOT have leaked into the table.
	for _, want := range []string{"", "https://push.example.com/no_auth"} {
		if _, err := st.GetPushSubscription(ctx, want); err == nil {
			t.Errorf("malformed push subscription leaked into store: %q", want)
		}
	}

	// vapid — kv has notify/vapid_public (plaintext) and notify/vapid_private
	// (envelope-encrypted with the auth/kek.bin KEK). The private row's
	// AAD must be "notify/vapid_private" so a runtime-side
	// vapidKVStore.LoadVAPID can re-open it.
	pubRec, err := st.GetKV(ctx, "notify", "vapid_public")
	if err != nil {
		t.Fatalf("get vapid_public: %v", err)
	}
	if pubRec.Value != "vapid-pub-fixture" || pubRec.Secret {
		t.Errorf("vapid_public = %+v", pubRec)
	}
	privRec, err := st.GetKV(ctx, "notify", "vapid_private")
	if err != nil {
		t.Fatalf("get vapid_private: %v", err)
	}
	if !privRec.Secret || len(privRec.ValueEncrypted) == 0 {
		t.Errorf("vapid_private not sealed: %+v", privRec)
	}
	// Round-trip the seal — the importer materializes the KEK at
	// <v1>/auth/kek.bin, so we re-load the same file and Open() must
	// recover the original v0 private key with the canonical AAD.
	kek, err := secretcrypto.LoadOrCreateKEK(filepath.Join(v1, "auth"))
	if err != nil {
		t.Fatalf("reload KEK: %v", err)
	}
	plain, err := secretcrypto.Open(kek, privRec.ValueEncrypted, []byte("notify/vapid_private"))
	if err != nil {
		t.Fatalf("open vapid_private: %v", err)
	}
	if string(plain) != "vapid-priv-fixture" {
		t.Errorf("vapid_private plaintext = %q, want vapid-priv-fixture", string(plain))
	}

	// external_chat_cursors — only thread-level JSONL files contribute
	// cursors. _channel.jsonl is intentionally excluded because v0's
	// FetchChannelHistory is a sliding-window fetch that never consults
	// LastPlatformTS as a delta cursor; importing it as a v1 cursor
	// would invite a future v1 channel poll to mistake the file's last
	// ts for a delta starting point and silently drop messages.
	//
	// Files whose lines are all bot/incomplete (no real platform
	// timestamp) are skipped — re-importing an empty cursor would still
	// be empty, and the v1 runtime's first poll re-fetches regardless.
	ecs, err := st.ListExternalChatCursorsByAgent(ctx, "ag_1")
	if err != nil {
		t.Fatalf("list external_chat_cursors ag_1: %v", err)
	}
	gotEC := map[string]string{}
	for _, r := range ecs {
		gotEC[r.ID] = r.Source + "|" + r.Cursor
	}
	wantEC := map[string]string{
		"ag_1:slack:C123:1712345678.000000": "slack|1712345678.500000",
	}
	if len(gotEC) != len(wantEC) {
		t.Errorf("external_chat_cursors = %v, want %v", gotEC, wantEC)
	}
	for k, v := range wantEC {
		if gotEC[k] != v {
			t.Errorf("external_chat_cursors[%s] = %q, want %q", k, gotEC[k], v)
		}
	}
	// _channel.jsonl must NOT have produced a row — sliding-window
	// fetch in v0 means its last ts isn't a real cursor.
	if _, err := st.GetExternalChatCursor(ctx, "ag_1:slack:C123"); err == nil {
		t.Errorf("_channel.jsonl leaked into external_chat_cursors store")
	}
	// Bot-only file's id must NOT have leaked into the store — confirms
	// the importer correctly skips files whose lastPlatformTS is empty.
	if _, err := st.GetExternalChatCursor(ctx, "ag_1:slack:C123:bot_only_thread"); err == nil {
		t.Errorf("bot-only thread cursor leaked into store")
	}
	// Channel-id population — every imported row must have channel_id
	// set (the schema permits NULL but the importer always knows the
	// channel).
	for _, r := range ecs {
		if r.ChannelID == nil || *r.ChannelID != "C123" {
			t.Errorf("external_chat_cursors[%s].channel_id = %v, want &C123", r.ID, r.ChannelID)
		}
	}

	// groupdm_messages
	gmsgs, err := st.ListGroupDMMessages(ctx, "gd_1", store.GroupDMMessageListOptions{})
	if err != nil {
		t.Fatalf("list gm: %v", err)
	}
	if len(gmsgs) != 2 {
		t.Fatalf("group msgs = %d, want 2", len(gmsgs))
	}

	// migration_status reflects "imported" for every domain (the
	// per-domain copier's terminal state — orchestrator-level
	// "complete" is set by migrate.Run, not the importers themselves).
	// The source_checksum column must be populated (non-empty hex
	// SHA256) since each domain in this fixture has at least one v0
	// file. The value is pinned across a re-run below so the audit
	// trail is stable.
	checksumBefore := map[string]string{}
	for _, dom := range []string{"agents", "messages", "groupdms", "tasks", "sessions", "notify_cursors", "vapid", "push_subscriptions", "external_chat_cursors", "compactions", "blobs"} {
		ph, err := migrate.PhaseOf(ctx, st, dom)
		if err != nil {
			t.Fatalf("phase %s: %v", dom, err)
		}
		if ph != "imported" {
			t.Errorf("phase[%s] = %q, want imported", dom, ph)
		}
		csum, err := migrate.ChecksumOf(ctx, st, dom)
		if err != nil {
			t.Fatalf("checksum %s: %v", dom, err)
		}
		if len(csum) != 64 {
			t.Errorf("checksum[%s] = %q (len %d), want 64-char hex", dom, csum, len(csum))
		}
		checksumBefore[dom] = csum
	}

	// Idempotency: a second run is a no-op. alreadyImported() returns
	// early without touching the row, so the checksum stamped on the
	// first run must persist verbatim (this is what the column is for —
	// audit history of what got imported, not a per-call recomputation).
	for _, imp := range importerOrder() {
		if err := imp.Run(ctx, st, opts); err != nil {
			t.Errorf("rerun %s: %v", imp.Domain(), err)
		}
	}
	for dom, want := range checksumBefore {
		got, err := migrate.ChecksumOf(ctx, st, dom)
		if err != nil {
			t.Fatalf("rerun checksum %s: %v", dom, err)
		}
		if got != want {
			t.Errorf("rerun checksum[%s] = %q, want %q", dom, got, want)
		}
	}
	agents2, _ := st.ListAgents(ctx)
	if len(agents2) != len(agents) {
		t.Errorf("rerun changed agents count: %d → %d", len(agents), len(agents2))
	}
	msgs2, _ := st.ListMessages(ctx, "ag_1", store.MessageListOptions{})
	if len(msgs2) != len(msgs) {
		t.Errorf("rerun changed messages count: %d → %d", len(msgs), len(msgs2))
	}
}

// TestImportAgentTasksEdgeCases exercises the status mapping fallbacks
// and resilience to malformed / empty input. Each of these should leave
// the importer in a clean "imported" state with a deterministic row
// count rather than aborting the whole migration.
func TestImportAgentTasksEdgeCases(t *testing.T) {
	v0 := t.TempDir()
	v1 := t.TempDir()
	writeV0Fixtures(t, v0)

	// Overwrite ag_1's tasks.json with a row mix that exercises every
	// branch of mapV0TaskStatus + the warn-and-skip paths.
	bad := `[
		{"id":"good_open","title":"open task","status":"open"},
		{"id":"good_done","title":"done task","status":"done"},
		{"id":"good_cancelled","title":"cancelled","status":"cancelled"},
		{"id":"weird","title":"weird","status":"snoozed"},
		{"id":"","title":"no id","status":"open"},
		{"id":"no_title","title":"","status":"open"}
	]`
	mustWrite := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	mustWrite(filepath.Join(v0, "agents", "ag_1", "tasks.json"), bad)

	// ag_2 — empty file (truncated tasks.json should be a no-op, not a
	// JSON parse failure).
	if err := os.WriteFile(filepath.Join(v0, "agents", "ag_2", "tasks.json"), nil, 0o644); err != nil {
		t.Fatalf("write empty: %v", err)
	}

	st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
	for _, imp := range importerOrder() {
		if err := imp.Run(ctx, st, opts); err != nil {
			t.Fatalf("importer %s: %v", imp.Domain(), err)
		}
	}

	tlist, err := st.ListAgentTasks(ctx, "ag_1", store.AgentTaskListOptions{})
	if err != nil {
		t.Fatalf("list ag_1: %v", err)
	}
	got := map[string]string{}
	for _, t := range tlist {
		got[t.ID] = t.Status
	}
	wantOK := map[string]string{
		"good_open":      "pending",
		"good_done":      "done",
		"good_cancelled": "cancelled",
	}
	for id, st := range wantOK {
		if got[id] != st {
			t.Errorf("task[%s].status = %q, want %q", id, got[id], st)
		}
	}
	for _, bad := range []string{"weird", "no_title"} {
		if _, exists := got[bad]; exists {
			t.Errorf("task[%s] should have been skipped (warn-and-continue)", bad)
		}
	}

	// ag_2's empty file is a no-op, not an error.
	if list, err := st.ListAgentTasks(ctx, "ag_2", store.AgentTaskListOptions{}); err != nil {
		t.Fatalf("list ag_2: %v", err)
	} else if len(list) != 0 {
		t.Errorf("ag_2 tasks = %d, want 0 (empty file)", len(list))
	}

	// Domain ends in "imported" with a populated checksum even when
	// every input row was malformed.
	if ph, err := migrate.PhaseOf(ctx, st, "tasks"); err != nil {
		t.Fatalf("phase: %v", err)
	} else if ph != "imported" {
		t.Errorf("phase = %q, want imported", ph)
	}
}

// TestImportAgentMessagesDedupesInFile feeds messages.jsonl with a
// repeated id and asserts the bulk path silently skips the duplicate
// instead of failing the whole batch on PK conflict — matching the
// per-row implementation that used `existing[m.ID] = true` after a
// successful AppendMessage and then skipped on the next encounter.
func TestImportAgentMessagesDedupesInFile(t *testing.T) {
	v0 := t.TempDir()
	v1 := t.TempDir()
	writeV0Fixtures(t, v0)

	// Append a duplicate row (same id as an earlier line) to ag_1's
	// messages.jsonl. The original three lines already cover m_a/m_b/m_c.
	dup := []byte(`{"id":"m_a","role":"user","content":"REPLAY","timestamp":"2026-04-01T11:00:00+09:00"}` + "\n")
	path := filepath.Join(v0, "agents", "ag_1", "messages.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open messages.jsonl: %v", err)
	}
	if _, err := f.Write(dup); err != nil {
		t.Fatalf("append dup: %v", err)
	}
	f.Close()

	st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
	for _, imp := range importerOrder() {
		if err := imp.Run(ctx, st, opts); err != nil {
			t.Fatalf("importer %s: %v", imp.Domain(), err)
		}
	}

	msgs, err := st.ListMessages(ctx, "ag_1", store.MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("dup not skipped: got %d msgs, want 3", len(msgs))
	}
	// First-write-wins: m_a's content stays "hi", not "REPLAY".
	for _, m := range msgs {
		if m.ID == "m_a" && m.Content != "hi" {
			t.Errorf("dup overwrote m_a: content=%q want %q", m.Content, "hi")
		}
	}
}

// TestImportAgentMessagesCrossAgentCollision regresses against the
// real user-reported failure where two agents had the same message
// id (typically because v0's `Fork --include-transcript` cloned
// messages.jsonl line-for-line). v1's agent_messages.id is a global
// PRIMARY KEY, so the second agent's import would crash on a UNIQUE
// constraint violation. The fix rewrites the colliding id to a
// fresh one before the bulk insert lands.
func TestImportAgentMessagesCrossAgentCollision(t *testing.T) {
	v0 := t.TempDir()
	v1 := t.TempDir()
	writeV0Fixtures(t, v0)

	// Make ag_2's first message share id "m_a" with ag_1's first
	// message — exactly the post-fork shape v0 produces.
	collidingLine := []byte(`{"id":"m_a","role":"user","content":"forked-from-ag_1","timestamp":"2026-04-01T11:00:00+09:00"}` + "\n")
	ag2Path := filepath.Join(v0, "agents", "ag_2", "messages.jsonl")
	if err := os.WriteFile(ag2Path, collidingLine, 0o644); err != nil {
		t.Fatalf("write ag_2 messages.jsonl: %v", err)
	}

	st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
	for _, imp := range importerOrder() {
		if err := imp.Run(ctx, st, opts); err != nil {
			t.Fatalf("importer %s: %v", imp.Domain(), err)
		}
	}

	// ag_1 still owns the original m_a (first import wins).
	ag1Msgs, err := st.ListMessages(ctx, "ag_1", store.MessageListOptions{})
	if err != nil {
		t.Fatalf("list ag_1: %v", err)
	}
	var ag1HasMA bool
	for _, m := range ag1Msgs {
		if m.ID == "m_a" {
			ag1HasMA = true
			if m.Content != "hi" {
				t.Errorf("ag_1.m_a content = %q; want %q (original)", m.Content, "hi")
			}
		}
	}
	if !ag1HasMA {
		t.Errorf("ag_1 lost m_a after collision rewrite")
	}

	// ag_2's collided message survived under a rewritten id with
	// the original content. The new id MUST start with "m_" and
	// MUST NOT equal "m_a".
	ag2Msgs, err := st.ListMessages(ctx, "ag_2", store.MessageListOptions{})
	if err != nil {
		t.Fatalf("list ag_2: %v", err)
	}
	if len(ag2Msgs) != 1 {
		t.Fatalf("ag_2 message count = %d; want 1", len(ag2Msgs))
	}
	got := ag2Msgs[0]
	if got.ID == "m_a" {
		t.Errorf("ag_2 message kept colliding id %q (rewrite did not fire)", got.ID)
	}
	if got.Content != "forked-from-ag_1" {
		t.Errorf("ag_2 content = %q; want %q", got.Content, "forked-from-ag_1")
	}
}

// TestImportPushSubscriptionsEdgeCases covers the broken-vapid +
// live-subs orphan-protection contract. Every case here has a v0
// push_subscriptions.json with at least one well-formed row whose
// vapid_public_key column would land in v1 referencing a key the
// runtime can no longer reproduce — so the migration MUST abort
// somewhere in the chain rather than silently markImported(0) on the
// vapid side and let push_subscriptions write rows under a key that
// will be regenerated on first boot.
//
// The fatal point is the vapid importer (which runs before
// push_subscriptions in importerOrder); push_subscriptions's own
// fatal-on-missing-vapid logic remains as a defense-in-depth fallback,
// covered by TestImportPushSubscriptionsEdgeCases_NoVapidImporter
// (intentionally not implemented — the chain enforces the contract,
// covering it twice would just couple two tests to one bug).
//
// The "good" case exercises the happy path: a well-formed vapid.json
// matched by well-formed subscriptions; both importers succeed.
func TestImportPushSubscriptionsEdgeCases(t *testing.T) {
	type vapidCase struct {
		name             string
		vapid            string // empty string ⇒ truncate; "<MISSING>" ⇒ remove
		wantVapidErr     bool   // expect vapid importer to abort (orphan-protection)
		wantPushSucceeds bool   // when wantVapidErr is false, expect push to land rows
	}
	cases := []vapidCase{
		{"good", `{"privateKey":"p","publicKey":"pub"}`, false, true},
		{"missing publicKey", `{"privateKey":"p"}`, true, false},
		{"missing privateKey", `{"publicKey":"pub"}`, true, false},
		{"empty publicKey", `{"privateKey":"p","publicKey":""}`, true, false},
		{"empty privateKey", `{"privateKey":"","publicKey":"pub"}`, true, false},
		{"malformed", `{ not json`, true, false},
		{"empty file", "", true, false},
		{"missing file", "<MISSING>", false, false}, // see comment in case body
	}
	mustWrite := func(t *testing.T, p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v0 := t.TempDir()
			v1 := t.TempDir()
			writeV0Fixtures(t, v0)

			vapidPath := filepath.Join(v0, "vapid.json")
			switch tc.vapid {
			case "<MISSING>":
				if err := os.Remove(vapidPath); err != nil {
					t.Fatalf("remove vapid.json: %v", err)
				}
			case "":
				// Empty file (writeV0Fixtures wrote a non-empty one;
				// truncate it).
				mustWrite(t, vapidPath, "")
			default:
				mustWrite(t, vapidPath, tc.vapid)
			}

			st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { st.Close() })

			ctx := context.Background()
			opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
			vapidErr := error(nil)
			pushErr := error(nil)
			vapidStopped := false
			for _, imp := range importerOrder() {
				if vapidStopped {
					// Once vapid errors, downstream importers don't
					// run — matches the production migrate.Run loop
					// which returns on any importer error.
					break
				}
				switch imp.Domain() {
				case "vapid":
					vapidErr = imp.Run(ctx, st, opts)
					if vapidErr != nil {
						vapidStopped = true
					}
				case "push_subscriptions":
					pushErr = imp.Run(ctx, st, opts)
				default:
					if err := imp.Run(ctx, st, opts); err != nil {
						t.Fatalf("setup importer %s: %v", imp.Domain(), err)
					}
				}
			}
			// "missing file" is a special case: vapid importer treats
			// os.ErrNotExist as markImported(0) without consulting the
			// orphan-check (push_subscriptions importer's own fatal on
			// missing vapid.json is the load-bearing guard for that
			// one branch). Verify push_subscriptions surfaces the
			// error in its place.
			if tc.name == "missing file" {
				if vapidErr != nil {
					t.Errorf("vapid importer should pass on missing vapid.json (push covers it), got %v", vapidErr)
				}
				if pushErr == nil {
					t.Errorf("push_subscriptions importer should fail on missing vapid.json")
				}
				return
			}
			if tc.wantVapidErr {
				if vapidErr == nil {
					t.Errorf("expected vapid importer error for %s", tc.name)
				}
				return
			}
			if vapidErr != nil {
				t.Errorf("unexpected vapid importer error: %v", vapidErr)
			}
			if tc.wantPushSucceeds && pushErr != nil {
				t.Errorf("push_subscriptions importer failed on good vapid: %v", pushErr)
			}
		})
	}

	// All-malformed push_subscriptions.json: vapid.json absence must
	// NOT block migration because there are no rows that need the key.
	t.Run("all malformed bypasses vapid check", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		writeV0Fixtures(t, v0)
		// Overwrite push_subscriptions.json with rows that all fail the
		// endpoint/auth/p256dh filter; remove vapid.json entirely.
		bad := `[
			{"endpoint":"","keys":{"auth":"a","p256dh":"p"}},
			{"endpoint":"x","keys":{"auth":"","p256dh":"p"}},
			{"endpoint":"y","keys":{"auth":"a","p256dh":""}}
		]`
		mustWrite(t, filepath.Join(v0, "push_subscriptions.json"), bad)
		if err := os.Remove(filepath.Join(v0, "vapid.json")); err != nil {
			t.Fatalf("remove vapid: %v", err)
		}

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { st.Close() })

		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		for _, imp := range importerOrder() {
			if err := imp.Run(ctx, st, opts); err != nil {
				t.Fatalf("importer %s: %v", imp.Domain(), err)
			}
		}
		if ph, err := migrate.PhaseOf(ctx, st, "push_subscriptions"); err != nil {
			t.Fatalf("phase: %v", err)
		} else if ph != "imported" {
			t.Errorf("phase = %q, want imported", ph)
		}
		active, err := st.ListActivePushSubscriptions(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(active) != 0 {
			t.Errorf("active = %d, want 0 (all rows malformed)", len(active))
		}
	})
}

// TestImportVAPIDEdgeCases covers the importer's tolerant posture on
// malformed / partial / absent vapid.json (markImported(0) without
// emitting any kv rows) and the auth/kek.bin materialization path.
// Distinct from push_subscriptions's edge cases — that domain is fatal
// on the same inputs because it cannot stamp a row's vapid_public_key
// without a valid public key, whereas the kv-side importer is content
// to leave the rows absent and let the runtime regenerate.
func TestImportVAPIDEdgeCases(t *testing.T) {
	type vapidCase struct {
		name      string
		vapid     string // empty ⇒ <MISSING> sentinel handled below
		wantPub   bool   // expect notify/vapid_public row to exist
		wantPriv  bool   // expect notify/vapid_private row to exist
	}
	cases := []vapidCase{
		{"good", `{"privateKey":"priv","publicKey":"pub"}`, true, true},
		{"missing publicKey", `{"privateKey":"p"}`, false, false},
		{"missing privateKey", `{"publicKey":"pub"}`, false, false},
		{"empty publicKey", `{"privateKey":"p","publicKey":""}`, false, false},
		{"malformed", `{ not json`, false, false},
		{"empty file", "", false, false},
		{"missing file", "<MISSING>", false, false},
	}
	mustWrite := func(t *testing.T, p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v0 := t.TempDir()
			v1 := t.TempDir()
			// Drop a minimal v0 fixture (no need for the full tree —
			// only the vapid importer is exercised here, so absent
			// agents/messages/etc. is fine; their own importers will
			// markImported(0) under empty input).
			vapidPath := filepath.Join(v0, "vapid.json")
			switch tc.vapid {
			case "<MISSING>":
				// no-op: file simply does not exist.
			case "":
				mustWrite(t, vapidPath, "")
			default:
				mustWrite(t, vapidPath, tc.vapid)
			}

			st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { st.Close() })

			ctx := context.Background()
			opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
			if err := (vapidImporter{}).Run(ctx, st, opts); err != nil {
				t.Fatalf("vapid importer: %v (must be permissive on malformed input)", err)
			}
			if ph, err := migrate.PhaseOf(ctx, st, "vapid"); err != nil {
				t.Fatalf("phase: %v", err)
			} else if ph != "imported" {
				t.Errorf("phase = %q, want imported", ph)
			}

			_, pubErr := st.GetKV(ctx, "notify", "vapid_public")
			_, privErr := st.GetKV(ctx, "notify", "vapid_private")
			if tc.wantPub {
				if pubErr != nil {
					t.Errorf("vapid_public missing: %v", pubErr)
				}
			} else {
				if !errors.Is(pubErr, store.ErrNotFound) {
					t.Errorf("vapid_public should be absent, got err=%v", pubErr)
				}
			}
			if tc.wantPriv {
				if privErr != nil {
					t.Errorf("vapid_private missing: %v", privErr)
				}
			} else {
				if !errors.Is(privErr, store.ErrNotFound) {
					t.Errorf("vapid_private should be absent, got err=%v", privErr)
				}
			}

			// Re-run is idempotent — phase stays "imported", no extra
			// rows materialize, kek.bin already on disk reused.
			if err := (vapidImporter{}).Run(ctx, st, opts); err != nil {
				t.Errorf("vapid importer rerun: %v", err)
			}
		})
	}
}

// TestImportVAPIDRejectsExistingMismatchedRow proves the importer
// verifies the existing kv row's value before treating an
// ErrETagMismatch as benign. A hand-bootstrapped v1 with a different
// VAPID public key in kv must NOT be silently overlaid with the v0
// pair — that would mark the migration imported with two halves
// pointing at different keys.
func TestImportVAPIDRejectsExistingMismatchedRow(t *testing.T) {
	v0 := t.TempDir()
	v1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(v0, "vapid.json"),
		[]byte(`{"privateKey":"v0-priv","publicKey":"v0-pub"}`), 0o644); err != nil {
		t.Fatalf("write vapid.json: %v", err)
	}

	st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	// Pre-seed kv with a DIFFERENT public key. Subsequent vapid
	// importer Run must refuse rather than markImported(2) over the
	// drift.
	if _, err := st.PutKV(ctx, &store.KVRecord{
		Namespace: "notify", Key: "vapid_public",
		Value: "preexisting-different-pub", Type: store.KVTypeString,
		Scope: store.KVScopeGlobal,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed pub: %v", err)
	}

	opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
	err = (vapidImporter{}).Run(ctx, st, opts)
	if err == nil {
		t.Fatal("expected refusal on mismatched existing vapid_public")
	}
}

// TestImportExternalChatCursorsEdgeCases pins the importer's behaviour
// across the off-the-happy-path branches that aren't naturally exercised
// by the round-trip fixture. The expected outcome differs per case:
//
//   - orphan agent dir (chat_history present, agent missing from
//     agents.json), empty platform/channel dirs, ':' in segment names,
//     missing agents.json, JSONL with no trailing newline, _channel.jsonl
//     exclusion → permissive: importer ends in "imported" with the
//     deterministic row count below, never aborts the migration.
//   - malformed / empty agents.json → fatal: matches the same posture
//     used by notify_cursors. Returning an empty agent set in those cases
//     would silently drop every cursor for every agent, which is worse
//     than surfacing the corruption.
func TestImportExternalChatCursorsEdgeCases(t *testing.T) {
	mustWrite := func(t *testing.T, p string, body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	t.Run("orphan agent dir is skipped", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		// agents.json with one valid agent ag_real; ag_orphan has a
		// chat_history dir on disk but no entry in agents.json.
		mustWrite(t, filepath.Join(v0, "agents", "agents.json"),
			[]byte(`[{"id":"ag_real","name":"R"}]`))
		mustWrite(t, filepath.Join(v0, "agents", "ag_real", "chat_history", "slack", "C1", "1.0.jsonl"),
			[]byte(`{"messageId":"1.0"}`+"\n"))
		mustWrite(t, filepath.Join(v0, "agents", "ag_orphan", "chat_history", "slack", "C9", "9.0.jsonl"),
			[]byte(`{"messageId":"9.0"}`+"\n"))

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err != nil {
			t.Fatalf("importer: %v", err)
		}
		if _, err := st.GetExternalChatCursor(ctx, "ag_real:slack:C1:1.0"); err != nil {
			t.Errorf("real cursor missing: %v", err)
		}
		if _, err := st.GetExternalChatCursor(ctx, "ag_orphan:slack:C9:9.0"); err == nil {
			t.Errorf("orphan agent cursor leaked into store")
		}
	})

	t.Run("empty platform and channel dirs are no-ops", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		mustWrite(t, filepath.Join(v0, "agents", "agents.json"),
			[]byte(`[{"id":"ag_x","name":"X"}]`))
		// Empty platform dir, empty channel dir — no JSONL files at all.
		if err := os.MkdirAll(filepath.Join(v0, "agents", "ag_x", "chat_history", "slack", "C_empty"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(v0, "agents", "ag_x", "chat_history", "discord_empty"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err != nil {
			t.Fatalf("importer: %v", err)
		}
		list, err := st.ListExternalChatCursorsByAgent(ctx, "ag_x")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("ag_x cursors = %d, want 0", len(list))
		}
	})

	t.Run("colon in channel and thread segments is rejected", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		mustWrite(t, filepath.Join(v0, "agents", "agents.json"),
			[]byte(`[{"id":"ag_y","name":"Y"}]`))
		// channel id with ':' — entire channel skipped
		mustWrite(t, filepath.Join(v0, "agents", "ag_y", "chat_history", "slack", "C:bad", "1.0.jsonl"),
			[]byte(`{"messageId":"1.0"}`+"\n"))
		// thread file with ':' — file skipped, channel kept
		mustWrite(t, filepath.Join(v0, "agents", "ag_y", "chat_history", "slack", "C_ok", "thr:bad.jsonl"),
			[]byte(`{"messageId":"1.0"}`+"\n"))
		mustWrite(t, filepath.Join(v0, "agents", "ag_y", "chat_history", "slack", "C_ok", "1.0.jsonl"),
			[]byte(`{"messageId":"1.0"}`+"\n"))

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err != nil {
			t.Fatalf("importer: %v", err)
		}
		if _, err := st.GetExternalChatCursor(ctx, "ag_y:slack:C_ok:1.0"); err != nil {
			t.Errorf("good cursor missing: %v", err)
		}
		// Colon-bearing segments must not produce rows under ANY id.
		list, err := st.ListExternalChatCursorsByAgent(ctx, "ag_y")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("ag_y rows = %d, want 1 (colon segments rejected)", len(list))
		}
	})

	t.Run("malformed agents.json is fatal", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		mustWrite(t, filepath.Join(v0, "agents", "agents.json"), []byte(`{not json`))

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err == nil {
			t.Errorf("expected fatal error on malformed agents.json")
		}
	})

	t.Run("empty agents.json is fatal", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		mustWrite(t, filepath.Join(v0, "agents", "agents.json"), nil)

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err == nil {
			t.Errorf("expected fatal error on empty agents.json")
		}
	})

	t.Run("missing agents.json is permissive", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		// chat_history dir exists but agents.json is absent — every
		// agent dir is "orphan" and the importer markImported(0)s.
		mustWrite(t, filepath.Join(v0, "agents", "ag_z", "chat_history", "slack", "C1", "1.0.jsonl"),
			[]byte(`{"messageId":"1.0"}`+"\n"))

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err != nil {
			t.Fatalf("importer should be permissive: %v", err)
		}
		if ph, err := migrate.PhaseOf(ctx, st, "external_chat_cursors"); err != nil {
			t.Fatalf("phase: %v", err)
		} else if ph != "imported" {
			t.Errorf("phase = %q, want imported", ph)
		}
	})

	t.Run("JSONL with no trailing newline still yields cursor", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		mustWrite(t, filepath.Join(v0, "agents", "agents.json"),
			[]byte(`[{"id":"ag_n","name":"N"}]`))
		// No trailing \n on the last line — bufio.Scanner still surfaces
		// it and the cursor must still resolve.
		mustWrite(t, filepath.Join(v0, "agents", "ag_n", "chat_history", "slack", "C1", "thr.jsonl"),
			[]byte(`{"messageId":"1.0"}`+"\n"+`{"messageId":"2.0"}`))

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err != nil {
			t.Fatalf("importer: %v", err)
		}
		got, err := st.GetExternalChatCursor(ctx, "ag_n:slack:C1:thr")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Cursor != "2.0" {
			t.Errorf("cursor = %q, want 2.0 (last line without newline)", got.Cursor)
		}
	})

	t.Run("_channel.jsonl is excluded", func(t *testing.T) {
		v0 := t.TempDir()
		v1 := t.TempDir()
		mustWrite(t, filepath.Join(v0, "agents", "agents.json"),
			[]byte(`[{"id":"ag_c","name":"C"}]`))
		mustWrite(t, filepath.Join(v0, "agents", "ag_c", "chat_history", "slack", "C1", "_channel.jsonl"),
			[]byte(`{"messageId":"1.0"}`+"\n"+`{"messageId":"2.0"}`+"\n"))

		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (externalChatCursorsImporter{}).Run(ctx, st, opts); err != nil {
			t.Fatalf("importer: %v", err)
		}
		list, err := st.ListExternalChatCursorsByAgent(ctx, "ag_c")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("ag_c rows = %d, want 0 (_channel.jsonl excluded)", len(list))
		}
	})
}

// TestImportCompactionsEdgeCases pins the no-op marker importer's three
// branches: absent dir / empty dir / hand-populated dir. All three end
// in phase="imported" with imported_count=0, but the source_checksum
// MUST differ between the empty and populated cases so a future re-run
// against a v0 dir that someone hand-populated surfaces as drift instead
// of vanishing into the no-op branch (which is the whole point of
// hashing files we don't parse/import).
func TestImportCompactionsEdgeCases(t *testing.T) {
	mustWrite := func(t *testing.T, p string, body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	run := func(t *testing.T, v0 string) string {
		t.Helper()
		v1 := t.TempDir()
		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (compactionsImporter{}).Run(ctx, st, opts); err != nil {
			t.Fatalf("importer: %v", err)
		}
		if ph, err := migrate.PhaseOf(ctx, st, "compactions"); err != nil {
			t.Fatalf("phase: %v", err)
		} else if ph != "imported" {
			t.Errorf("phase = %q, want imported", ph)
		}
		// imported_count must always be 0 — this is a marker importer,
		// every code path goes through markImported(..., 0, ...).
		if cnt, err := migrate.CountOf(ctx, st, "compactions"); err != nil {
			t.Fatalf("count: %v", err)
		} else if cnt != 0 {
			t.Errorf("imported_count = %d, want 0 (no rows are ever emitted)", cnt)
		}
		csum, err := migrate.ChecksumOf(ctx, st, "compactions")
		if err != nil {
			t.Fatalf("checksum: %v", err)
		}
		if len(csum) != 64 {
			t.Errorf("checksum len = %d, want 64", len(csum))
		}
		// Idempotent re-run leaves the row untouched.
		if err := (compactionsImporter{}).Run(ctx, st, opts); err != nil {
			t.Errorf("rerun: %v", err)
		}
		return csum
	}

	t.Run("absent compactions dir", func(t *testing.T) {
		v0 := t.TempDir()
		_ = run(t, v0)
	})

	t.Run("empty compactions dir", func(t *testing.T) {
		v0 := t.TempDir()
		if err := os.MkdirAll(filepath.Join(v0, "compactions"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_ = run(t, v0)
	})

	t.Run("populated dir produces different checksum", func(t *testing.T) {
		// Two v0 dirs that differ only by the presence of a file under
		// compactions/. Their domain checksums MUST differ — that's the
		// drift detection contract.
		empty := t.TempDir()
		if err := os.MkdirAll(filepath.Join(empty, "compactions"), 0o755); err != nil {
			t.Fatalf("mkdir empty: %v", err)
		}
		populated := t.TempDir()
		mustWrite(t, filepath.Join(populated, "compactions", "stale.md"),
			[]byte("hand-placed content\n"))

		csumEmpty := run(t, empty)
		csumPopulated := run(t, populated)
		if csumEmpty == csumPopulated {
			t.Errorf("checksum identical for empty vs populated compactions dir: %s", csumEmpty)
		}
	})

	t.Run("symlink leaf is skipped (warned, not hashed)", func(t *testing.T) {
		// A symlink under compactions/ must not be followed. walkDirV0
		// returns an injected ErrNotRegular for it; the importer logs
		// + skips, and the leaf is excluded from source_checksum.
		//
		// To prove the symlink target's bytes did NOT enter the
		// fingerprint, we compute the checksum twice — once with the
		// symlink pointing at one external file, once with it pointing
		// at a different external file — and assert both equal the
		// empty-dir baseline. If the symlink were followed, the two
		// runs would diverge (different target bytes → different
		// sha256), and would also differ from the baseline.
		baseline := t.TempDir()
		if err := os.MkdirAll(filepath.Join(baseline, "compactions"), 0o755); err != nil {
			t.Fatalf("mkdir baseline: %v", err)
		}
		csumBaseline := run(t, baseline)

		mkV0WithLink := func(targetBody string) string {
			t.Helper()
			v0 := t.TempDir()
			if err := os.MkdirAll(filepath.Join(v0, "compactions"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			external := t.TempDir()
			mustWrite(t, filepath.Join(external, "secret.md"), []byte(targetBody))
			linkPath := filepath.Join(v0, "compactions", "linked.md")
			if err := os.Symlink(filepath.Join(external, "secret.md"), linkPath); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			return v0
		}
		csumA := run(t, mkV0WithLink("payload-A"))
		csumB := run(t, mkV0WithLink("payload-B"))
		if csumA != csumBaseline {
			t.Errorf("symlink leaf influenced checksum: got %s, want %s (baseline empty dir)", csumA, csumBaseline)
		}
		if csumA != csumB {
			t.Errorf("symlink leaf checksum varied with target body: A=%s B=%s (target was followed?)", csumA, csumB)
		}
	})

	t.Run("symlinked compactions root is rejected", func(t *testing.T) {
		// A symlink AS the compactions dir itself would otherwise let
		// walkDirV0 traverse outside V0Dir entirely. The lstat-and-
		// reject branch in collectCompactionsSourcePaths must surface
		// ErrNotRegular and abort the importer rather than silently
		// hashing nothing (which would look identical to "absent dir").
		v0 := t.TempDir()
		external := t.TempDir()
		mustWrite(t, filepath.Join(external, "stale.md"), []byte("outside"))
		if err := os.Symlink(external, filepath.Join(v0, "compactions")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}

		v1 := t.TempDir()
		st, err := store.Open(context.Background(), store.Options{ConfigDir: v1})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		ctx := context.Background()
		opts := migrate.Options{V0Dir: v0, V1Dir: v1, HomePeer: "peer-test"}
		if err := (compactionsImporter{}).Run(ctx, st, opts); err == nil {
			t.Errorf("expected error on symlinked compactions root")
		} else if !errors.Is(err, ErrNotRegular) {
			t.Errorf("err = %v, want chain containing ErrNotRegular", err)
		}
		if ph, err := migrate.PhaseOf(ctx, st, "compactions"); err != nil {
			t.Fatalf("phase: %v", err)
		} else if ph == "imported" {
			t.Errorf("phase = imported on rejected root, want unfinished (got %q)", ph)
		}
	})
}

func writeV0Fixtures(t *testing.T, v0 string) {
	t.Helper()
	mustMkdir := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	mustWrite := func(p string, data []byte) {
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	agentsBase := filepath.Join(v0, "agents")
	mustMkdir(agentsBase)

	// sessions.json — covers the v0 → v1 force-archived path. One row
	// claims status="running" (which v0 may legitimately persist if
	// kojo crashed without flushing the post-stop transition); the
	// importer must demote it to archived. exitCode is preserved when
	// present so the v1 row carries the post-mortem signal. A row
	// without an id is intentionally skipped to confirm the warn-and-
	// skip path stays aligned with the messages/tasks importers.
	exit0 := 0
	sessions := []map[string]any{
		{"id": "sess_alive", "tool": "claude", "workDir": "/tmp/x", "args": []string{"--dev"},
			"status": "running", "createdAt": "2026-04-01T10:00:00+09:00"},
		{"id": "sess_done", "tool": "codex", "workDir": "/tmp/y",
			"status": "stopped", "exitCode": exit0, "createdAt": "2026-04-02T10:00:00+09:00"},
		{"id": "", "tool": "gemini", "workDir": "/tmp/z", "status": "stopped"},
	}
	sessionsJSON, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("marshal sessions.json: %v", err)
	}
	mustWrite(filepath.Join(v0, "sessions.json"), sessionsJSON)

	// notify_cursors.json — exercises every branch of the importer:
	//   - "ag_1:src_slack" / "ag_1:src_gmail": resolvable via agents.json
	//     → mapped to "ag_1:slack:src_slack" / "ag_1:gmail:src_gmail"
	//   - "ag_1:orphan_src": no matching NotifySources entry → orphan,
	//     skipped with a warn log (must not abort the whole import)
	//   - "ag_1:src_empty" with empty cursor → skipped (haven't-polled-yet
	//     sentinel from v0; no value to migrate)
	cursorsJSON, err := json.Marshal(map[string]string{
		"ag_1:src_slack":  "slack-cursor-1",
		"ag_1:src_gmail":  "gmail-cursor-1",
		"ag_1:orphan_src": "orphan-cursor",
		"ag_1:src_empty":  "",
	})
	if err != nil {
		t.Fatalf("marshal notify_cursors.json: %v", err)
	}
	mustWrite(filepath.Join(v0, "notify_cursors.json"), cursorsJSON)

	// vapid.json — provides the public-key column for push_subscriptions.
	// The private key is recorded but not used by the push importer
	// (envelope-encrypted vapid kv import is a separate slice).
	vapidJSON, err := json.Marshal(map[string]string{
		"privateKey": "vapid-priv-fixture",
		"publicKey":  "vapid-pub-fixture",
	})
	if err != nil {
		t.Fatalf("marshal vapid.json: %v", err)
	}
	mustWrite(filepath.Join(v0, "vapid.json"), vapidJSON)

	// push_subscriptions.json — exercises the importer's branches:
	//   - "ep_a" / "ep_b": well-formed → imported with vapid_public_key
	//     copied from vapid.json
	//   - missing endpoint: malformed → warn-skipped (mirrors v0's
	//     loadSubscriptions cleanup)
	//   - missing keys.auth: malformed → warn-skipped
	subs := []map[string]any{
		{"endpoint": "https://push.example.com/ep_a", "keys": map[string]string{"auth": "auth-a", "p256dh": "p256-a"}},
		{"endpoint": "https://push.example.com/ep_b", "keys": map[string]string{"auth": "auth-b", "p256dh": "p256-b"}},
		{"endpoint": "", "keys": map[string]string{"auth": "x", "p256dh": "y"}},
		{"endpoint": "https://push.example.com/no_auth", "keys": map[string]string{"auth": "", "p256dh": "z"}},
	}
	subsJSON, err := json.Marshal(subs)
	if err != nil {
		t.Fatalf("marshal push_subscriptions.json: %v", err)
	}
	mustWrite(filepath.Join(v0, "push_subscriptions.json"), subsJSON)

	// agents.json
	agentsJSON, err := json.Marshal([]map[string]any{
		{
			"id":              "ag_1",
			"name":            "Alice",
			"persona":         "would be ignored — persona.md wins",
			"model":           "sonnet",
			"tool":            "claude",
			"intervalMinutes": 30,
			"createdAt":       "2026-04-01T10:00:00+09:00",
			"updatedAt":       "2026-04-02T10:00:00+09:00",
			// notifySources feeds the notify_cursors importer's
			// (agentID, sourceID) → sourceType lookup. Two declared
			// sources → matching cursor entries below get an inferred
			// type; a third cursor with an unknown sourceID exercises
			// the orphan-skip path.
			"notifySources": []map[string]any{
				{"id": "src_slack", "type": "slack", "enabled": true, "intervalMinutes": 5},
				{"id": "src_gmail", "type": "gmail", "enabled": true, "intervalMinutes": 10},
			},
		},
		{
			"id":        "ag_2",
			"name":      "Bob",
			"persona":   "inline only",
			"createdAt": "2026-04-01T10:00:00+09:00",
			"updatedAt": "2026-04-01T10:00:00+09:00",
		},
	})
	if err != nil {
		t.Fatalf("marshal agents.json: %v", err)
	}
	mustWrite(filepath.Join(agentsBase, "agents.json"), agentsJSON)

	// ag_1 layout
	a1 := filepath.Join(agentsBase, "ag_1")
	mustMkdir(filepath.Join(a1, "memory", "projects"))
	mustMkdir(filepath.Join(a1, "memory", "people"))
	mustMkdir(filepath.Join(a1, "memory", "topics"))
	mustMkdir(filepath.Join(a1, "memory", "archive"))
	mustWrite(filepath.Join(a1, "persona.md"), []byte("alice persona body\n"))
	mustWrite(filepath.Join(a1, "MEMORY.md"), []byte("MEMORY index for alice\n"))
	// daily files at top level
	mustWrite(filepath.Join(a1, "memory", "2026-04-01.md"), []byte("daily 1\n"))
	mustWrite(filepath.Join(a1, "memory", "2026-04-02.md"), []byte("daily 2\n"))
	// non-date top-level → topic
	mustWrite(filepath.Join(a1, "memory", "loose.md"), []byte("loose topic file\n"))
	// canonical subdirs
	mustWrite(filepath.Join(a1, "memory", "projects", "kojo.md"), []byte("project kojo\n"))
	mustWrite(filepath.Join(a1, "memory", "people", "akari.md"), []byte("people akari\n"))
	mustWrite(filepath.Join(a1, "memory", "topics", "release.md"), []byte("topic release\n"))
	mustWrite(filepath.Join(a1, "memory", "archive", "2026-03.md"), []byte("archived march\n"))
	// messages.jsonl
	msgs := []map[string]any{
		{"id": "m_a", "role": "user", "content": "hi", "timestamp": "2026-04-01T10:00:00+09:00"},
		{"id": "m_b", "role": "user", "content": "second", "timestamp": "2026-04-01T10:01:00+09:00"},
		{"id": "m_c", "role": "assistant", "content": "reply", "timestamp": "2026-04-01T10:02:00+09:00"},
	}
	var msgsBuf []byte
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		msgsBuf = append(msgsBuf, b...)
		msgsBuf = append(msgsBuf, '\n')
	}
	mustWrite(filepath.Join(a1, "messages.jsonl"), msgsBuf)

	// ag_1 tasks.json — covers the v0 → v1 status mapping (open → pending,
	// done → done) and the position-based seq allocation. The third entry
	// uses the v1 vocabulary directly to confirm that pre-v1 hand edits
	// pass through unchanged rather than being clobbered.
	tasks := []map[string]any{
		{"id": "task_a", "title": "ship release", "status": "open",
			"createdAt": "2026-04-01T09:00:00+09:00", "updatedAt": "2026-04-01T09:30:00+09:00"},
		{"id": "task_b", "title": "write docs", "status": "done",
			"createdAt": "2026-04-01T09:05:00+09:00", "updatedAt": "2026-04-02T10:00:00+09:00"},
		{"id": "task_c", "title": "draft notes", "status": "in_progress",
			"createdAt": "2026-04-02T10:00:00+09:00", "updatedAt": "2026-04-02T10:30:00+09:00"},
	}
	tasksBuf, err := json.Marshal(tasks)
	if err != nil {
		t.Fatalf("marshal tasks.json: %v", err)
	}
	mustWrite(filepath.Join(a1, "tasks.json"), tasksBuf)

	// ag_1 blob artefacts (avatar/books/outbox/temp/index/credentials).
	// Coverage targets each scope/walk arm of blobsImporter so the
	// round-trip exercises the global / local / machine paths and the
	// directory walks for books/outbox/temp/index.
	mustWrite(filepath.Join(a1, "avatar.png"), []byte{0x89, 'P', 'N', 'G'})
	mustMkdir(filepath.Join(a1, "books"))
	mustWrite(filepath.Join(a1, "books", "intro.md"), []byte("intro book\n"))
	mustWrite(filepath.Join(a1, "books", "ch01.md"), []byte("chapter one\n"))
	mustMkdir(filepath.Join(a1, "outbox"))
	mustWrite(filepath.Join(a1, "outbox", "draft.txt"), []byte("draft body\n"))
	mustMkdir(filepath.Join(a1, "temp"))
	mustWrite(filepath.Join(a1, "temp", "scratch.json"), []byte(`{"k":"v"}`))
	mustMkdir(filepath.Join(a1, "index"))
	mustWrite(filepath.Join(a1, "index", "memory.db"), []byte("SQLITE-FAKE-BLOB"))
	mustWrite(filepath.Join(a1, "credentials.json"), []byte(`{"token":"x"}`))
	mustWrite(filepath.Join(a1, "credentials.key"), []byte("ENVELOPE-KEY"))

	// Catchall coverage: regression for Phase C importer scope
	// expansion. v0 agents accumulate top-level scratch files,
	// outputs/, arbitrary subdirs (projects/research/etc.) — none of
	// which were enumerated in the original mapping. Every regular
	// file added here lands in the blob store at scope=local
	// addressed under agents/<id>/<rel>; suppressed files (skip
	// branches) MUST NOT generate a blob_refs row.
	mustWrite(filepath.Join(a1, "game-design-doc.md"), []byte("scratch design doc\n"))
	mustMkdir(filepath.Join(a1, "outputs"))
	mustWrite(filepath.Join(a1, "outputs", "result.bvh"), []byte("BVH-FAKE\n"))
	mustWrite(filepath.Join(a1, "outputs", "timing.json"), []byte(`{"t":1}`))
	mustMkdir(filepath.Join(a1, "projects", "kojo-v1"))
	mustWrite(filepath.Join(a1, "projects", "kojo-v1", "notes.md"), []byte("project notes\n"))
	mustMkdir(filepath.Join(a1, "research", "x-cookies"))
	mustWrite(filepath.Join(a1, "research", "x-cookies", "session.json"), []byte(`{"sid":"abc"}`))
	// Suppressed leaves — each exercises a distinct skip rule.
	mustWrite(filepath.Join(a1, "MEMORY.md.bak"), []byte("backup file"))
	mustWrite(filepath.Join(a1, "messages.jsonl.bak2"), []byte("backup file 2"))
	mustWrite(filepath.Join(a1, "persona_summary.md"), []byte("regenerable summary"))
	mustWrite(filepath.Join(a1, ".DS_Store"), []byte("DS"))
	mustWrite(filepath.Join(a1, "kojo.lock"), []byte("lock"))
	mustMkdir(filepath.Join(a1, ".claude", "captures"))
	mustWrite(filepath.Join(a1, ".claude", "settings.local.json"), []byte("{}"))
	mustMkdir(filepath.Join(a1, ".codex"))
	mustWrite(filepath.Join(a1, ".codex", "session.json"), []byte("{}"))
	mustMkdir(filepath.Join(a1, ".gemini"))
	mustWrite(filepath.Join(a1, ".gemini", "settings.json"), []byte("{}"))

	// chat_history layout — exercises the external_chat_cursors importer:
	//   - _channel.jsonl:                      excluded channel rollup (NOT a cursor source)
	//   - 1712345678.000000.jsonl:             per-thread cursor (with one bot reply at the tail)
	//   - bot_only_thread.jsonl:               all bot lines → empty cursor → skipped
	// The channel rollup file is dropped on the importer side (sliding-
	// window fetch in v0 means its last ts is not a cursor). Threaded
	// files use LastPlatformTS, which only honors all-numeric-with-dots
	// ids; the bot reply tail must NOT advance the cursor past the last
	// real Slack ts.
	chatBase := filepath.Join(a1, "chat_history", "slack", "C123")
	mustMkdir(chatBase)
	chMsgs := []map[string]any{
		{"platform": "slack", "channelId": "C123", "messageId": "1712345678.100000", "text": "first"},
		{"platform": "slack", "channelId": "C123", "messageId": "1712345678.200000", "text": "second"},
		{"platform": "slack", "channelId": "C123", "messageId": "1712345999.bot", "text": "bot tail", "isBot": true},
	}
	var chBuf []byte
	for _, m := range chMsgs {
		b, _ := json.Marshal(m)
		chBuf = append(chBuf, b...)
		chBuf = append(chBuf, '\n')
	}
	mustWrite(filepath.Join(chatBase, "_channel.jsonl"), chBuf)
	thrMsgs := []map[string]any{
		{"platform": "slack", "channelId": "C123", "threadId": "1712345678.000000",
			"messageId": "1712345678.300000", "text": "reply 1"},
		{"platform": "slack", "channelId": "C123", "threadId": "1712345678.000000",
			"messageId": "1712345678.500000", "text": "reply 2"},
		{"platform": "slack", "channelId": "C123", "threadId": "1712345678.000000",
			"messageId": "1712345678.999.bot", "text": "bot tail", "isBot": true},
	}
	var thrBuf []byte
	for _, m := range thrMsgs {
		b, _ := json.Marshal(m)
		thrBuf = append(thrBuf, b...)
		thrBuf = append(thrBuf, '\n')
	}
	mustWrite(filepath.Join(chatBase, "1712345678.000000.jsonl"), thrBuf)
	// Bot-only thread — every line fails isNumericTSChars, so
	// LastPlatformTS returns "" and the importer skips this file entirely.
	botMsgs := []map[string]any{
		{"platform": "slack", "channelId": "C123", "threadId": "bot_only_thread",
			"messageId": "1712345700.bot", "text": "bot 1", "isBot": true},
		{"platform": "slack", "channelId": "C123", "threadId": "bot_only_thread",
			"messageId": "_incomplete", "text": "incomplete marker"},
	}
	var botBuf []byte
	for _, m := range botMsgs {
		b, _ := json.Marshal(m)
		botBuf = append(botBuf, b...)
		botBuf = append(botBuf, '\n')
	}
	mustWrite(filepath.Join(chatBase, "bot_only_thread.jsonl"), botBuf)

	// ag_2 — inline persona only, no per-agent dir contents
	mustMkdir(filepath.Join(agentsBase, "ag_2"))

	// groupdms
	gdir := filepath.Join(agentsBase, "groupdms")
	mustMkdir(gdir)
	groupsJSON, err := json.Marshal([]map[string]any{
		{
			"id":      "gd_1",
			"name":    "alice×bob",
			"members": []map[string]any{{"agentId": "ag_1", "agentName": "Alice"}, {"agentId": "ag_2", "agentName": "Bob"}},
			"style":   "efficient",
			"venue":   "chatroom",
			"createdAt": "2026-04-01T10:00:00+09:00",
			"updatedAt": "2026-04-01T10:00:00+09:00",
		},
	})
	if err != nil {
		t.Fatalf("marshal groups.json: %v", err)
	}
	mustWrite(filepath.Join(gdir, "groups.json"), groupsJSON)

	g1 := filepath.Join(gdir, "gd_1")
	mustMkdir(g1)
	gmsgs := []map[string]any{
		{"id": "gm_a", "agentId": "ag_1", "agentName": "Alice", "content": "yo", "timestamp": "2026-04-01T10:00:00+09:00"},
		{"id": "gm_b", "agentId": "ag_2", "agentName": "Bob", "content": "sup", "timestamp": "2026-04-01T10:01:00+09:00"},
	}
	var gbuf []byte
	for _, m := range gmsgs {
		b, _ := json.Marshal(m)
		gbuf = append(gbuf, b...)
		gbuf = append(gbuf, '\n')
	}
	mustWrite(filepath.Join(g1, "messages.jsonl"), gbuf)
}
