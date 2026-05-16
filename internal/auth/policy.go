package auth

import (
	"net/http"
	"strings"
)

// EnforceMiddleware gates /api/v1/* requests by Principal/method/path
// using AllowNonOwner. It runs after AuthMiddleware (which set the
// Principal in ctx). Static files and non-API paths pass through.
func EnforceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := FromContext(r.Context())
		if !p.IsOwner() && strings.HasPrefix(r.URL.Path, "/api/v1/") {
			if !AllowNonOwner(p, r.Method, r.URL.Path) {
				http.Error(w, `{"error":{"code":"forbidden","message":"forbidden"}}`, http.StatusForbidden)
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
// This wrapper applies on the *agent-facing* (auth-required) listener
// only. The public listener uses OwnerOnlyMiddleware and never enters
// this gate.
//
// The intent is "default deny". Routes are grouped into three buckets:
//
//   1. Public reads — info, directory, agent list/get, avatar.
//   2. Self-scoped reads/writes — files, messages, tasks, credentials,
//      memory, slackbot config, notify sources, group memberships,
//      avatar upload, persona / metadata patch, MCP, PreCompact hook.
//      Permitted only when the path's {id} matches the principal.
//   3. Privileged-cross-agent — delete / reset / checkin / unarchive /
//      reset-session. Permitted for self by Agent, or for any target by
//      PrivAgent.
//
// Owner-only routes (sessions, git, files browser, oauth, embedding,
// push, custom-models, group DM mutate-as-owner, fork, /privilege,
// generate-*) fall through and 403. Note: POST /api/v1/groupdms
// (group creation) is exposed to Agent / PrivAgent below — the handler
// then enforces the caller-in-memberIds invariant.
func AllowNonOwner(p Principal, method, path string) bool {
	if p.IsOwner() {
		return true
	}

	// WebDAV mount surface: the handler-side `webdavGate` is the
	// canonical authorization point here because the credential a
	// WebDAV client presents is HTTP Basic — AuthMiddleware can't
	// resolve it to a Principal (only Bearer / X-Kojo-Token go
	// through there), so we'd 403 a perfectly valid Basic-auth
	// mount request before it ever reached the gate.
	//
	// Letting any principal through to the mux means the gate has
	// to enforce both "no creds → 401 with Basic challenge" and
	// "bad creds → 401" itself, which it does. The blast radius is
	// bounded: only /api/v1/webdav/* opts out of policy-layer
	// enforcement.
	if strings.HasPrefix(path, "/api/v1/webdav/") || path == "/api/v1/webdav" {
		return true
	}

	// RoleWebDAV is otherwise a dead-end — the token's only valid
	// destination is the webdav mount above. Any other API path
	// falls through to default-deny.
	if p.IsWebDAV() {
		return false
	}

	// RolePeer is scoped to the inter-peer surface (status push
	// feed for §3.10, blob handoff for §3.7). Every other API
	// path falls through to default-deny — a leaked peer signing
	// key can't reach /api/v1/agents or any other route. Method
	// gating is explicit so a future mutation handler under the
	// same prefix doesn't inherit the read allowlist.
	if p.IsPeer() {
		if method == http.MethodGet {
			if path == "/api/v1/peers/events" {
				return true
			}
			if strings.HasPrefix(path, "/api/v1/peers/blobs/") {
				return true
			}
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
		// Blanket agent-path proxy surface (§3.7 device-switch):
		// Hub's remoteAgentProxyMiddleware forwards any agent-
		// scoped request from an authorised caller (Owner /
		// Agent) to the holder peer, signing it with the Hub's
		// Ed25519 identity. The target's handler still runs
		// its own guards (If-Match, busy checks, lock holder,
		// etc.). Loop prevention in the middleware ensures we
		// never re-proxy a peer-signed request. Subsumes the
		// earlier per-endpoint entries (WS, messages).
		if strings.HasPrefix(path, "/api/v1/agents/") {
			return true
		}
		return false
	}

	// Bare /api/v1/info — reduced view (version only) returned by the handler.
	if method == http.MethodGet && path == "/api/v1/info" {
		return true
	}
	// Peer list: agents need this to discover handoff targets
	// by Tailscale machine name. The handler returns a reduced
	// view (no public_key, no capabilities) for non-Owner
	// principals so identity material doesn't leak. Other
	// /api/v1/peers/* routes (POST, DELETE, /self, /rotate-key)
	// stay owner-only via the handler-level gate.
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
	if method == http.MethodGet && matchAgentSubpath(path, "") {
		return true
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
	case "/files", "/files/view", "/files/raw":
		return method == http.MethodGet
	case "/messages":
		return method == http.MethodGet
	case "/tasks":
		return method == http.MethodGet || method == http.MethodPost
	case "/credentials":
		return method == http.MethodGet || method == http.MethodPost
	case "/notify-sources":
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
	}

	// Sub-resources with their own {id} segments (messages, tasks,
	// credentials, notify-sources). Match conservatively.
	switch {
	case strings.HasPrefix(sub, "/messages/"):
		return method == http.MethodPatch || method == http.MethodDelete || method == http.MethodPost
	case strings.HasPrefix(sub, "/tasks/"):
		return method == http.MethodPatch || method == http.MethodDelete
	case strings.HasPrefix(sub, "/credentials/"):
		return method == http.MethodGet || method == http.MethodPatch || method == http.MethodDelete || method == http.MethodPost
	case strings.HasPrefix(sub, "/notify-sources/"):
		return method == http.MethodGet || method == http.MethodPatch || method == http.MethodDelete
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

// matchAgentSubpath checks /api/v1/agents/{id}{sub} where sub is the
// expected suffix ("" for the bare GET /agents/{id}). Used in the
// allowlist for read-only endpoints exposed to Agent principals.
func matchAgentSubpath(path, expectedSub string) bool {
	_, sub, ok := SplitAgentIDPath(path)
	if !ok {
		return false
	}
	return sub == expectedSub
}
