package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// writeJSONError writes the {"error":{"code","message"}} envelope used
// by internal/server's writeError, but from the auth package so the
// middleware refusals carry Content-Type: application/json instead of
// the text/plain (plus trailing newline) that http.Error emits. The
// status/code/message are the caller's; the wire shape matches
// server.writeError exactly (map marshals "code" before "message").
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// allowPeerSessionPath returns true when (method, path) names a
// known RolePeer-callable session route. Explicit list — additions
// require a deliberate edit here. Covers every endpoint registered
// by registerRoutes under /api/v1/sessions; /api/v1/ws is handled
// by the caller because its query semantics differ.
//
// Routes:
//
//	GET    /api/v1/sessions                           list
//	POST   /api/v1/sessions                           create
//	GET    /api/v1/sessions/{id}                      info
//	DELETE /api/v1/sessions/{id}                      stop
//	PATCH  /api/v1/sessions/{id}                      yolo toggle / patch
//	POST   /api/v1/sessions/{id}/restart
//	POST   /api/v1/sessions/{id}/tmux
//	GET    /api/v1/sessions/{id}/terminal
//	GET    /api/v1/sessions/{id}/attachments
//	DELETE /api/v1/sessions/{id}/attachments          ?path=
func allowPeerSessionPath(method, path string) bool {
	if path == "/api/v1/sessions" {
		return method == http.MethodGet || method == http.MethodPost
	}
	const prefix = "/api/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	// Strip the {id} segment.
	slash := strings.IndexByte(rest, '/')
	var sub string
	if slash >= 0 {
		sub = rest[slash:] // e.g. "/restart", "/attachments"
	}
	switch sub {
	case "":
		// /api/v1/sessions/{id}
		switch method {
		case http.MethodGet, http.MethodPatch, http.MethodDelete:
			return true
		}
	case "/restart", "/tmux":
		return method == http.MethodPost
	case "/terminal":
		return method == http.MethodGet
	case "/attachments":
		return method == http.MethodGet || method == http.MethodDelete
	}
	return false
}

// EnforceMiddleware gates /api/v1/* requests by Principal/method/path
// using AllowNonOwner. It runs after AuthMiddleware (which set the
// Principal in ctx). Static files and non-API paths pass through.
func EnforceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := FromContext(r.Context())
		if !p.IsOwner() && strings.HasPrefix(r.URL.Path, "/api/v1/") {
			if !AllowNonOwner(p, r.Method, r.URL.Path) {
				writeJSONError(w, http.StatusForbidden, "forbidden", "forbidden")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// AllowNonOwner gates non-Owner principals against a small whitelist of
// API routes. Unrecognised paths are denied with 403, and recognised
// paths may further be denied based on Principal/method/target.
//
// Wired via EnforceMiddleware on BOTH listeners:
//
//   - public (tsnet) listener — TailnetIdentityMiddleware stamps
//     RoleOwner for every tailnet caller (Hub mode), including
//     callers whose WhoIs is transiently unresolved, or RolePeer /
//     Guest (peer mode). Owner short-circuits at the top, so this
//     gate effectively runs only for the §3.7 RolePeer inter-peer
//     surface and Guest fallthroughs.
//   - agent-facing (auth-required) loopback listener — AuthMiddleware
//     resolves Bearer/X-Kojo-Token to Owner/Agent/PrivAgent,
//     and this gate enforces per-route policy for non-Owner roles.
//
// The intent is "default deny". Routes are grouped into three buckets:
//
//  1. Public reads — info, directory, agent list/get, avatar.
//  2. Self-scoped reads/writes — files, messages, tasks, credentials,
//     memory, slackbot config, notify sources, group memberships,
//     avatar upload, persona / metadata patch, MCP, PreCompact hook.
//     Permitted only when the path's {id} matches the principal.
//  3. Privileged-cross-agent — delete / reset / checkin / unarchive /
//     reset-session. Permitted for self by Agent, or for any target by
//     PrivAgent.
//
// Owner-only routes (sessions, git, files browser, embedding,
// push, custom-models, group DM mutate-as-owner, fork, /privilege,
// generate-*) fall through and 403. Note: POST /api/v1/groupdms
// (group creation) is exposed to Agent / PrivAgent below — the handler
// then enforces the caller-in-memberIds invariant.
func AllowNonOwner(p Principal, method, path string) bool {
	if p.IsOwner() {
		return true
	}

	// RolePeer is scoped to the inter-peer surface (status push
	// feed for §3.10, blob handoff for §3.7, device-switch
	// orchestration). The principal is stamped by
	// TailnetIdentityMiddleware when the request arrives over the
	// peer's tsnet listener and WhoIs resolves to a peer_registry
	// row — Tailnet identity IS the trust signal. The legacy
	// Ed25519-signed Bearer envelope path has been retired; no
	// other RolePeer stamping route exists. Every API path
	// outside this block falls through to default-deny, so an
	// unpaired tailnet caller (Guest after WhoIs) can't reach
	// /api/v1/agents or any other route. Method gating is
	// explicit so a future mutation handler under the same
	// prefix doesn't inherit the read allowlist.
	if p.IsPeer() {
		if method == http.MethodGet {
			if path == "/api/v1/peers/events" {
				return true
			}
			if strings.HasPrefix(path, "/api/v1/peers/blobs/") {
				return true
			}
		}
		if method == http.MethodHead &&
			strings.HasPrefix(path, "/api/v1/peers/blobs/") {
			// HEAD is the metadata-only twin of the GET above —
			// hub's kojo-attach live-read fallback uses it to
			// probe size / ETag before deciding whether to relay
			// the body. Admit alongside GET so HEAD doesn't 403
			// while GET succeeds.
			return true
		}
		if method == http.MethodPost && path == "/api/v1/peers/pull" {
			// Device-switch orchestration (§3.7 step 4): the
			// Hub signs the pull dispatch as its own peer
			// identity; the target's policy admits it as
			// RolePeer. No other peer-auth POST surface is
			// allowed — keep the method/path check tight so a
			// future handler under /api/v1/peers/* doesn't
			// inherit POST access by accident.
			return true
		}
		if method == http.MethodPost && path == "/api/v1/peers/agent-sync" {
			// Device-switch agent metadata push (§3.7 step
			// 4-bis): source peer signs the payload, target
			// applies via store.SyncAgentFromPeer. Same trust
			// model as /peers/pull.
			return true
		}
		if method == http.MethodPost &&
			(path == "/api/v1/peers/agent-sync/finalize" ||
				path == "/api/v1/peers/agent-sync/drop") {
			// Two-phase agent-sync companions. finalize
			// activates target runtime state after a
			// successful complete; drop rolls back on abort.
			return true
		}
		if method == http.MethodPost && path == "/api/v1/peers/agent-sync/state" {
			// Incremental device-switch preflight (§3.7): source
			// peer asks target for its high-water marks so the
			// agent-sync payload ships only the delta. Read-only
			// at the row level (no mutations on target);
			// signer-equals-source and agent_lock holder check
			// run inside the handler.
			return true
		}
		// kojo-attach hub-ingest path.
		if method == http.MethodPut && strings.HasPrefix(path, "/api/v1/peers/blobs-ingest/") {
			return true
		}
		// Every paired peer (= has a peer_registry row matched by
		// WhoIs over tsnet) is admitted on the privileged surface.
		// The earlier "trusted" bit that gated this is gone —
		// Tailnet identity (registry membership) IS the operator's
		// trust signal. --unsafe collapses the WhoIs check and
		// unconditionally stamps RolePeer on every caller (LAN /
		// docker / CI escape hatch).
		if strings.HasPrefix(path, "/api/v1/agents/") {
			return true
		}
		if allowPeerSessionPath(method, path) {
			return true
		}
		if method == http.MethodGet && path == "/api/v1/ws" {
			return true
		}
		if method == http.MethodGet && path == "/api/v1/info" {
			return true
		}
		if method == http.MethodGet && path == "/api/v1/dirs" {
			return true
		}
		// File browser + raw fetch + upload. Mirrors the routes the
		// Hub UI hits when it has selected a remote peer in the
		// session screen's File/Attachments tabs.
		if method == http.MethodGet && (path == "/api/v1/files" ||
			path == "/api/v1/files/view" || path == "/api/v1/files/raw") {
			return true
		}
		if method == http.MethodPost && path == "/api/v1/upload" {
			return true
		}
		// Git surface used by the Git tab. Read-only routes admit
		// GET; the exec endpoint runs whitelisted operations
		// inside handler-side guards.
		if method == http.MethodGet && (path == "/api/v1/git/status" ||
			path == "/api/v1/git/log" || path == "/api/v1/git/diff") {
			return true
		}
		if method == http.MethodPost && path == "/api/v1/git/exec" {
			return true
		}
		return false
	}

	// Bare /api/v1/info — reduced view (version only) returned by the handler.
	if method == http.MethodGet && path == "/api/v1/info" {
		return true
	}
	// Peer list: agents need this to discover handoff targets
	// by Tailscale machine name. The wire shape carries no
	// identity-sensitive fields, so Owner and Agent see the same
	// response. Mutating /api/v1/peers/* routes (POST, PATCH,
	// DELETE) and /self stay owner-only via the handler-level
	// gate.
	if method == http.MethodGet && path == "/api/v1/peers" && p.IsAgent() {
		return true
	}
	// Public agent reads.
	if method == http.MethodGet && path == "/api/v1/agents/directory" {
		return true
	}
	if method == http.MethodGet && path == "/api/v1/agents" {
		return true
	}
	if method == http.MethodGet && matchAgentSubpath(path) {
		return true
	}

	// Daemon self-restart — privileged agents only (the handler
	// re-checks via CanRestartServer). Owner is admitted by the
	// IsOwner short-circuit at the top; RolePeer never reaches
	// here (peer branch returned above).
	if method == http.MethodPost && path == "/api/v1/system/restart" {
		return p.Role == RolePrivAgent
	}

	// Per-agent {id} routes.
	if id, sub, ok := SplitAgentIDPath(path); ok {
		// Avatar and agent-active status are public-readable.
		if method == http.MethodGet && (sub == "/avatar" || sub == "/active") {
			return true
		}
		// WebSocket endpoint binds the calling agent to a chat session;
		// the upgrade handler still enforces its own origin check. We
		// allow only self-scoped agents on /api/v1/agents/{id}/ws so a
		// stray Guest cannot tail another agent's transcript stream.
		if sub == "/ws" {
			return p.IsAgent() && p.AgentID == id
		}
		// Privileged cross-agent operations on the bare agent path.
		switch method {
		case http.MethodDelete:
			if sub == "" {
				return p.CanDeleteOrReset(id)
			}
		case http.MethodPost:
			switch sub {
			case "/reset", "/unarchive", "/checkin", "/reset-session", "/memory/truncate":
				return p.CanDeleteOrReset(id)
			case "/fork", "/privilege":
				// Owner-only — already filtered out above.
				return false
			case "/handoff/switch":
				// Agent-self orchestrated device switch
				// (§3.7). Only the agent itself may move
				// itself between peers — Owner is handled by
				// the IsOwner short-circuit at the top of
				// AllowNonOwner. Other agents must NOT be
				// able to migrate someone else's data.
				return p.IsAgent() && p.AgentID == id
			}
		}
		// Everything else is self-scoped read/write. The handler still
		// applies its own access checks; this layer just refuses
		// requests that target a different agent.
		if !p.IsAgent() || p.AgentID != id {
			return false
		}
		return isSelfScopedRoute(method, sub)
	}

	// Bare `POST /api/v1/groupdms` (group creation) — agents may create
	// groups they themselves are a member of. The handler enforces the
	// caller-in-memberIds invariant; the policy layer only refuses
	// non-Agent principals here.
	if path == "/api/v1/groupdms" && method == http.MethodPost && p.IsAgent() {
		return true
	}

	// `POST /api/v1/dms` (find-or-create 1:1 DM) — same contract as group
	// creation: the handler enforces caller-in-memberIds.
	if path == "/api/v1/dms" && method == http.MethodPost && p.IsAgent() {
		return true
	}

	// Group DM API — allow members to read groups, post messages, and
	// manage their own membership. The handler is responsible for
	// confirming the principal is actually a member of the named
	// group (and that {agentId} on member-scoped routes matches the
	// caller). The user-messages endpoint stays Owner-only.
	if strings.HasPrefix(path, "/api/v1/groupdms/") && p.IsAgent() {
		// Strip the static prefix and the {id} segment.
		rest := path[len("/api/v1/groupdms/"):]
		var sub string
		if idx := strings.Index(rest, "/"); idx >= 0 {
			sub = rest[idx:]
		}
		switch {
		case sub == "" && (method == http.MethodGet || method == http.MethodPatch || method == http.MethodDelete):
			// PATCH/DELETE are still Owner-only at the handler layer for
			// group-wide ops, but the policy layer surfaces 403 vs the
			// finer-grained check there.
			return method == http.MethodGet
		case sub == "/messages" && (method == http.MethodGet || method == http.MethodPost):
			return true
		case sub == "/unread" && method == http.MethodGet:
			// Member agents may poll their own unread counters; the
			// handler enforces membership.
			return true
		case strings.HasPrefix(sub, "/members"):
			return true
		}
		return false
	}

	// Top-level WebSocket /api/v1/ws is the (legacy) global session
	// stream; refuse Bearer principals — only Owner reaches it.
	if path == "/api/v1/ws" {
		return false
	}

	return false
}

// isSelfScopedRoute returns true for sub-paths under /api/v1/agents/{id}
// that an agent may invoke against itself. Anything not listed here
// falls through to default-deny.
func isSelfScopedRoute(method, sub string) bool {
	switch sub {
	case "":
		// PATCH /api/v1/agents/{id} — handler enforces self check too.
		return method == http.MethodPatch
	case "/avatar":
		// POST upload, GET handled above.
		return method == http.MethodPost
	case "/avatar/generated":
		return method == http.MethodPost
	case "/files", "/files/view", "/files/raw", "/files/thumb":
		return method == http.MethodGet
	case "/messages":
		// GET: agent reads its own transcript. POST (HTTP send /
		// queue-and-forward delivery) is Owner- or RolePeer-driven
		// — an agent has no reason to HTTP-post to its own chat
		// loop, so keep it denied here.
		return method == http.MethodGet
	case "/queued-messages":
		// Queue-and-forward inspection is Owner-only (handler
		// re-checks); explicit deny for Agent principals.
		return false
	case "/tasks":
		return method == http.MethodGet || method == http.MethodPost
	case "/credentials":
		return method == http.MethodGet || method == http.MethodPost
	case "/slackbot":
		return method == http.MethodGet || method == http.MethodPut || method == http.MethodDelete
	case "/groups":
		return method == http.MethodGet
	case "/mcp":
		// MCP transport — agent's own tool surface.
		return true
	case "/pre-compact":
		// PreCompact hook fired by claude-code on the agent's own session.
		return method == http.MethodPost
	case "/user-context":
		// Agent reads/writes its own user.md workspace file. GET surfaces
		// the in-memory DefaultUserContent template for new agents (no
		// disk persistence until the first PUT). PUT lands at the
		// per-agent path under agentDir(id).
		return method == http.MethodGet || method == http.MethodPut
	case "/status":
		// Agent reads/writes its own status.json workspace file (the
		// self-maintained state injected into the system prompt tail).
		return method == http.MethodGet || method == http.MethodPut
	case "/checkin-file":
		// Agent reads/writes its own checkin.md workspace file. GET
		// surfaces DefaultCheckinContent when checkin.md is absent so
		// the settings UI shows the same body the cron path would run.
		return method == http.MethodGet || method == http.MethodPut
	}

	// Sub-resources with their own {id} segments (messages, tasks,
	// credentials). Match conservatively.
	switch {
	case strings.HasPrefix(sub, "/messages/"):
		return method == http.MethodPatch || method == http.MethodDelete || method == http.MethodPost
	case strings.HasPrefix(sub, "/tasks/"):
		return method == http.MethodPatch || method == http.MethodDelete
	case strings.HasPrefix(sub, "/credentials/"):
		return method == http.MethodGet || method == http.MethodPatch || method == http.MethodDelete || method == http.MethodPost
	}
	return false
}

// SplitAgentIDPath parses /api/v1/agents/{id}{sub} into (id, sub, true)
// where sub starts with "/" or is empty. Returns ok=false if the path
// does not match.
func SplitAgentIDPath(path string) (id, sub string, ok bool) {
	const prefix = "/api/v1/agents/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	if rest == "" || rest == "directory" || rest == "generate-persona" || rest == "generate-name" || rest == "generate-avatar" || rest == "preview-avatar" || rest == "cron-paused" {
		return "", "", false
	}
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rest, "", true
	}
	return rest[:slash], rest[slash:], true
}

// matchAgentSubpath reports whether path is the bare GET /api/v1/agents/{id}
// route (no trailing sub-segment). Used in the allowlist for read-only
// endpoints exposed to Agent principals.
func matchAgentSubpath(path string) bool {
	_, sub, ok := SplitAgentIDPath(path)
	if !ok {
		return false
	}
	return sub == ""
}
