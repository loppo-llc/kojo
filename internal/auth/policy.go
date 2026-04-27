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
// push, custom-models, group DM CRUD, fork, /privilege, generate-*)
// fall through and 403.
func AllowNonOwner(p Principal, method, path string) bool {
	if p.IsOwner() {
		return true
	}

	// Bare /api/v1/info — reduced view (version only) returned by the handler.
	if method == http.MethodGet && path == "/api/v1/info" {
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
	if id, sub, ok := splitAgentIDPath(path); ok {
		// Avatar is public-readable.
		if method == http.MethodGet && sub == "/avatar" {
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
			case "/reset", "/unarchive", "/checkin", "/reset-session":
				return p.CanDeleteOrReset(id)
			case "/fork", "/privilege":
				// Owner-only — already filtered out above.
				return false
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

	// Group DM API — allow members to read groups, post messages, and
	// manage their own membership. The handler is responsible for
	// confirming the principal is actually a member of the named
	// group (and that {agentId} on member-scoped routes matches the
	// caller). Bare `POST /api/v1/groupdms` (group creation) and the
	// user-messages endpoint stay Owner-only.
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

// splitAgentIDPath parses /api/v1/agents/{id}{sub} into (id, sub, true)
// where sub starts with "/" or is empty. Returns ok=false if the path
// does not match.
func splitAgentIDPath(path string) (id, sub string, ok bool) {
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
	_, sub, ok := splitAgentIDPath(path)
	if !ok {
		return false
	}
	return sub == expectedSub
}
