package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// Read-through of a remote-held agent's record.
//
// While an agent's runtime lock is held by another peer, every
// holder-owned settings write (PATCH /agents/{id}, proxied by
// remoteAgentProxyMiddleware) lands on the holder, and this hub's
// agents row only catches up inside the next device-switch sync.
// Serving that row verbatim made a just-saved setting (cronExpr,
// model, tool, persona, ...) appear to revert on reload, and the
// Settings form's full-payload save then wrote the stale values back.
// Reads therefore go to the holder while it is reachable; this hub's
// row is the fallback only.
//
// A few fields are owned by this hub regardless of placement and are
// overlaid on the holder's view:
//   - placement: holderPeer / holderPeerName / holderPeerStatus
//   - hub-only management routes (never proxied): slackBot,
//     privileged, ownerDeputy, lastTransferSkips(+Generation)
//   - hub-local-safe edits made while the holder was offline, projected
//     with the same 3-way merge the next sync ingest applies
//     (Manager.ProjectHubLocalOverrides)

// holderAgentReadTimeout bounds one holder read so an unresponsive
// peer degrades to the hub row instead of stalling the page.
const holderAgentReadTimeout = 5 * time.Second

// holderAgentBodyLimit caps the holder's agent JSON we buffer.
const holderAgentBodyLimit = 8 << 20

// hubOwnedAgentKeys are the JSON keys writeHolderAgentView takes from
// the merged (hub-overlaid) agent instead of the holder's raw body.
var hubOwnedAgentKeys = []string{
	"holderPeer", "holderPeerName", "holderPeerStatus",
	"slackBot", "privileged", "ownerDeputy",
	"lastTransferSkips", "lastTransferSkipsGeneration",
	// hub-local-safe fields (pending offline overrides)
	"name", "publicProfile", "publicProfileOverride", "effort", "autoEffort",
	"disabledInjections", "silentStart", "silentEnd", "notifyDuringSilent",
}

// fetchHolderAgent reads GET /api/v1/agents/{id} from hub.HolderPeer.
// It returns the raw JSON object (preserving response-only fields
// such as etag / nextCronAt, and fields newer than this binary) plus
// its decoded Agent. ok=false when the holder is offline, unknown,
// slow, answers non-200, or returns a body that is not this agent.
func (s *Server) fetchHolderAgent(ctx context.Context, hub *agent.Agent) (map[string]json.RawMessage, *agent.Agent, bool) {
	if hub == nil || hub.HolderPeer == "" || s.peerID == nil || s.agents.Store() == nil {
		return nil, nil, false
	}
	peerRec, err := s.agents.Store().GetPeer(ctx, hub.HolderPeer)
	if err != nil || peerRec == nil || peerRec.Status != store.PeerStatusOnline {
		return nil, nil, false
	}
	addr, err := peer.NormalizeAddress(peerRec.URL)
	if err != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, holderAgentReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/v1/agents/"+hub.ID, nil)
	if err != nil {
		return nil, nil, false
	}
	resp, err := peer.NoKeepAliveHTTPClient(holderAgentReadTimeout).Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("holder agent read failed; using hub row",
				"agent", hub.ID, "peer", hub.HolderPeer, "err", err)
		}
		return nil, nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if s.logger != nil {
			s.logger.Debug("holder agent read non-200; using hub row",
				"agent", hub.ID, "peer", hub.HolderPeer, "status", resp.StatusCode)
		}
		return nil, nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, holderAgentBodyLimit))
	if err != nil {
		return nil, nil, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return nil, nil, false
	}
	var holder agent.Agent
	if json.Unmarshal(body, &holder) != nil || holder.ID != hub.ID {
		// Misrouted holder / stale registry URL: never show another
		// agent's record under this ID.
		return nil, nil, false
	}
	return raw, &holder, true
}

// mergeHolderAgent returns a copy of the holder's view with the
// hub-owned fields (see the file comment) taken from this hub.
func (s *Server) mergeHolderAgent(ctx context.Context, hub, holder *agent.Agent) *agent.Agent {
	m := *holder
	s.agents.ProjectHubLocalOverrides(ctx, &m)
	m.HolderPeer = hub.HolderPeer
	m.HolderPeerName = hub.HolderPeerName
	m.HolderPeerStatus = hub.HolderPeerStatus
	m.SlackBot = hub.SlackBot
	m.Privileged = hub.Privileged
	m.OwnerDeputy = hub.OwnerDeputy
	m.LastTransferSkips = hub.LastTransferSkips
	m.LastTransferSkipsGeneration = hub.LastTransferSkipsGeneration
	return &m
}

// writeHolderAgentView serves GET /agents/{id} for a remote-held agent
// from its holder. full selects the owner/self record (holder body
// with hub-owned keys overlaid, holder row etag kept so a proxied
// PATCH If-Match matches the holder's row) versus the directory view.
// No HTTP ETag header is emitted: this hub cannot 304 against a body
// it did not build. Returns false, having written nothing, when the
// holder cannot be read.
func (s *Server) writeHolderAgentView(w http.ResponseWriter, r *http.Request, hub *agent.Agent, full bool) bool {
	raw, holder, ok := s.fetchHolderAgent(r.Context(), hub)
	if !ok {
		return false
	}
	merged := s.mergeHolderAgent(r.Context(), hub, holder)
	if !full {
		writeJSONResponse(w, http.StatusOK, toDirectoryView(merged))
		return true
	}
	mb, err := json.Marshal(merged)
	if err != nil {
		return false
	}
	var mm map[string]json.RawMessage
	if json.Unmarshal(mb, &mm) != nil {
		return false
	}
	for _, k := range hubOwnedAgentKeys {
		if v, present := mm[k]; present {
			raw[k] = v
		} else {
			delete(raw, k) // omitempty zero on the hub side
		}
	}
	// A switch can be in flight on either side (this hub orchestrates
	// it; the holder is the departing source): report it if either
	// one says so.
	if s.agents.IsSwitching(hub.ID) {
		raw["isSwitching"] = json.RawMessage("true")
	}
	writeJSONResponse(w, http.StatusOK, raw)
	return true
}
