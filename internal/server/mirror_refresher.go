package server

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// remote_message_mirror push-refresher (§3.7 device-switch).
//
// Background: the mirror is normally written only on a *successful*
// proxy GET of messages (proxyPeerGetMessages / fetchRemoteLatestMessage),
// which means it goes stale exactly when it's needed — the holder drops
// offline and reads fall back to whatever the last client-triggered GET
// left behind. This refresher closes that gap by proactively fetching
// recent messages from each online holder on a jittered interval and
// upserting them via the exact same code path the proxy GET uses
// (Manager.UpsertMirrorFromMessages → UpsertRemoteMirrorMessagesIfHolder).
//
// Safety properties, all inherited from the existing write path:
//   - agents that come back local: the store-level holder check inside
//     UpsertRemoteMirrorMessagesIfHolder skips the upsert when
//     agent_locks no longer names the fetched-from peer (or the row is
//     gone), so a refresh racing a force-reclaim can't resurrect stale
//     rows. The next enumeration pass drops the agent entirely because
//     ListAgentLocksNotHeldBy no longer returns it.
//   - offline peers are never contacted: refreshMirrorForAgent checks
//     peer_registry status == online before dialing, same as
//     proxyPeerGetMessages.
//   - clean shutdown: runMirrorRefresher is started by New() and stopped
//     by Shutdown() closing mirrorRefreshDone, mirroring the
//     runChunkedSyncSweeper lifecycle.
const (
	// mirrorRefreshInterval is the base cadence between refresh sweeps.
	// Modest on purpose: the mirror only needs to be "recent enough" for
	// the offline read fallback, not live.
	mirrorRefreshInterval = 60 * time.Second
	// mirrorRefreshJitter is added (0..jitter) to each sleep so multiple
	// hubs / restarts don't synchronise their fetch bursts.
	mirrorRefreshJitter = 15 * time.Second
	// mirrorRefreshFetchLimit bounds the per-agent message fetch. Matches
	// the default page size the chat UI requests via the proxy path.
	mirrorRefreshFetchLimit = 50
	// mirrorRefreshTimeout bounds one holder round-trip.
	mirrorRefreshTimeout = 10 * time.Second
	// mirrorRefreshSweepTimeout bounds one whole sweep so a run over
	// many slow-but-"online" holders can't starve the cadence forever.
	mirrorRefreshSweepTimeout = 2 * time.Minute
)

// runMirrorRefresher periodically refreshes the remote message mirror
// for every agent held by another peer. Started by New(); stopped by
// Shutdown closing mirrorRefreshDone.
func (s *Server) runMirrorRefresher() {
	if s.mirrorRefreshStopped != nil {
		// Ack channel: Shutdown waits on this before closing the store
		// so an in-flight sweep's mirror upsert never races the DB close.
		defer close(s.mirrorRefreshStopped)
	}
	for {
		d := mirrorRefreshInterval + rand.N(mirrorRefreshJitter)
		t := time.NewTimer(d)
		select {
		case <-s.mirrorRefreshDone:
			t.Stop()
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), mirrorRefreshSweepTimeout)
		// Abort an in-flight sweep promptly if Shutdown fires mid-pass.
		stop := make(chan struct{})
		go func() {
			select {
			case <-s.mirrorRefreshDone:
				cancel()
			case <-stop:
			}
		}()
		s.refreshRemoteMirrors(ctx)
		close(stop)
		cancel()
	}
}

// refreshRemoteMirrors runs one sweep: enumerate agents whose lock is
// held by another peer and refresh each one's mirror from its holder.
// Best effort — individual failures are logged at debug and skipped.
// Tests call this directly instead of waiting for the ticker.
func (s *Server) refreshRemoteMirrors(ctx context.Context) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return
	}
	st := s.agents.Store()
	locks, err := st.ListAgentLocksNotHeldBy(ctx, s.peerID.DeviceID)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("mirrorRefresher: list remote locks failed", "err", err)
		}
		return
	}
	for _, lk := range locks {
		if ctx.Err() != nil {
			return
		}
		s.refreshMirrorForAgent(ctx, lk.AgentID, lk.HolderPeer)
	}
}

// refreshMirrorForAgent fetches the most recent messages for one agent
// from its holder peer and upserts them into the mirror. No-op when
// the holder is unknown or not online (never dials offline peers).
func (s *Server) refreshMirrorForAgent(ctx context.Context, agentID, holderDeviceID string) {
	st := s.agents.Store()
	peerRec, err := st.GetPeer(ctx, holderDeviceID)
	if err != nil || peerRec == nil || peerRec.Status != store.PeerStatusOnline {
		return
	}
	addr, err := peer.NormalizeAddress(peerRec.URL)
	if err != nil {
		return
	}
	targetURL := addr + "/api/v1/agents/" + agentID + "/messages?limit=" +
		strconv.Itoa(mirrorRefreshFetchLimit)

	ctx, cancel := context.WithTimeout(ctx, mirrorRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return
	}
	// Peer identity is carried in the WireGuard tunnel and resolved
	// server-side via tsnet WhoIs — same as fetchRemoteLatestMessage,
	// no token header needed for hub→holder fetches.
	client := peer.NoKeepAliveHTTPClient(mirrorRefreshTimeout)
	resp, err := client.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("mirrorRefresher: fetch failed",
				"agent", agentID, "peer", holderDeviceID, "err", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if s.logger != nil {
			s.logger.Debug("mirrorRefresher: non-200 from holder",
				"agent", agentID, "peer", holderDeviceID, "status", resp.StatusCode)
		}
		return
	}
	var body struct {
		Messages []*agent.Message `json:"messages"`
		HasMore  bool             `json:"hasMore"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		if s.logger != nil {
			s.logger.Debug("mirrorRefresher: decode failed",
				"agent", agentID, "peer", holderDeviceID, "err", err)
		}
		return
	}
	if len(body.Messages) == 0 && body.HasMore {
		// Defensive: a holder should never claim "0 messages but more
		// exist" — don't treat that as an empty transcript.
		return
	}
	// Replace-window semantics (not plain upsert): this fetch is the
	// holder's authoritative newest-N snapshot, so mirror rows inside
	// that window that the holder no longer returned were deleted on
	// the holder and must be pruned — otherwise deleted messages
	// resurrect in the offline read fallback. Empty messages with
	// hasMore=false clears the mirror entirely. Rows older than the
	// window (from deeper paginated proxy reads) are preserved.
	if err := s.agents.ReplaceMirrorWindowFromMessages(agentID, holderDeviceID, body.Messages); err != nil && s.logger != nil {
		s.logger.Debug("mirrorRefresher: mirror replace failed",
			"agent", agentID, "peer", holderDeviceID, "err", err)
	}
}
