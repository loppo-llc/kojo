package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// This file unifies the shared skeleton of the six cross-peer proxy
// sites (§3.7 device switch + session peer routing). Each site does
// resolve-peer → NormalizeAddress → dial → stream, but with
// deliberately different policies (WS vs HTTP, timeouts, header
// stripping, error statuses/bodies). The helpers below capture the
// mechanical skeleton; every per-site difference is passed in as an
// explicit option so the six policies read side by side at the call
// sites.
//
// Three sites (proxySessionWebSocket, proxySessionRequest,
// proxyCreateSessionToPeer) share the exact same resolve error bodies
// and use resolvePeerDialAddr. The other three (proxyAgentWebSocket,
// proxyToHolderPeer, relayPeerBlob) carry bespoke resolve error
// policies (agent-lock wording, online check, ErrNotFound split) and
// keep their resolve prefix inline, then hand off to the shared
// dial/stream tail.
//
// Split reported: proxyCreateSessionToPeer buffers + JSON-restamps its
// response instead of streaming, so only its resolve prefix is shared;
// its tail stays inline.

// resolvePeerDialAddr looks up deviceID in the peer registry and
// normalizes it to a dial address, writing the standard 502 bodies on
// failure and returning ok=false. Shared by the three sites whose
// resolve error bodies are byte-identical ("target peer not in
// registry" / "target peer has no usable dial address").
func (s *Server) resolvePeerDialAddr(w http.ResponseWriter, ctx context.Context, st *store.Store, deviceID string) (string, bool) {
	rec, err := st.GetPeer(ctx, deviceID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer not in registry: "+err.Error())
		return "", false
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer has no usable dial address: "+err.Error())
		return "", false
	}
	return addr, true
}

// peerHTTPForward is one HTTP proxy site's policy. forwardHTTPToPeer
// runs the shared build-request → dial → stream tail; the fields below
// express each site's differences.
type peerHTTPForward struct {
	method        string
	url           string
	body          io.Reader
	contentLength int64 // assigned verbatim to the outbound request

	// Outbound request headers: for each key, copy srcHeader.Get(key)
	// when non-empty. srcHeader may be nil when reqHeaderKeys is empty.
	srcHeader     http.Header
	reqHeaderKeys []string

	// timeout is passed to peer.NoKeepAliveHTTPClient (0 = no cap).
	timeout time.Duration

	// build-request failure policy. buildErrPrefix == "" emits the bare
	// err.Error() (no prefix).
	buildErrStatus int
	buildErrCode   string
	buildErrPrefix string

	// dial (client.Do) failure policy. onDialErr, if set, runs before
	// writeError (for per-site logging).
	dialErrStatus int
	dialErrCode   string
	dialErrPrefix string
	onDialErr     func(err error)

	// respHeaderKeys are echoed from the upstream response onto the
	// client response before the status + streamed body.
	respHeaderKeys []string
}

// forwardHTTPToPeer performs the shared HTTP proxy tail: build the
// outbound request, dial with a keep-alive-disabled client, copy the
// allow-listed response headers, then stream the body back. All
// per-site policy lives in o.
func (s *Server) forwardHTTPToPeer(w http.ResponseWriter, ctx context.Context, o peerHTTPForward) {
	req, err := http.NewRequestWithContext(ctx, o.method, o.url, o.body)
	if err != nil {
		msg := err.Error()
		if o.buildErrPrefix != "" {
			msg = o.buildErrPrefix + err.Error()
		}
		writeError(w, o.buildErrStatus, o.buildErrCode, msg)
		return
	}
	req.ContentLength = o.contentLength
	for _, h := range o.reqHeaderKeys {
		if v := o.srcHeader.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	// Keep-alives disabled: a fresh TCP/TLS handshake per hop avoids
	// silent nonce replay on stale idle connections (see the retired
	// Ed25519-signing path) and is cheap relative to the transfer.
	client := peer.NoKeepAliveHTTPClient(o.timeout)
	resp, err := client.Do(req)
	if err != nil {
		if o.onDialErr != nil {
			o.onDialErr(err)
		}
		writeError(w, o.dialErrStatus, o.dialErrCode, o.dialErrPrefix+err.Error())
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header, o.respHeaderKeys...)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// peerWSForward is one WebSocket proxy site's policy. forwardWebSocketToPeer
// runs the shared parse → scheme-rewrite → dial → accept → pipe tail.
type peerWSForward struct {
	addr  string     // normalized http(s) dial address
	path  string     // target request path
	query url.Values // nil = leave RawQuery untouched

	// build-upgrade-request failure: writes 500 internal. "" prefix
	// emits the bare err.Error().
	buildErrPrefix string

	// dial (websocket.Dial) failure: writes 502 bad_gateway with
	// dialErrPrefix+err. onDialErr, if set, runs before writeError.
	dialErrPrefix string
	onDialErr     func(err error)

	// onAcceptErr runs when the inbound (UI) upgrade fails; the caller
	// logs and the request ends (no body written — the upgrade already
	// consumed the response).
	onAcceptErr func(err error)

	// onClosed runs after both directions finish (per-site debug log).
	onClosed func()
}

// forwardWebSocketToPeer dials the target peer's WS endpoint over
// tsnet (target's ServeAuthTsnet stamps RolePeer from Tailnet
// identity, so no Authorization header is needed), then upgrades the
// inbound UI socket and pipes frames both ways until either side
// closes. The target is dialed FIRST so a target-side reject surfaces
// as a clean HTTP error before the inbound conn is upgraded.
func (s *Server) forwardWebSocketToPeer(w http.ResponseWriter, r *http.Request, o peerWSForward) {
	targetURL, err := url.Parse(o.addr)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target address unparseable: "+err.Error())
		return
	}
	if !rewriteHTTPSchemeToWS(w, targetURL) {
		return
	}
	targetURL.Path = o.path
	if o.query != nil {
		targetURL.RawQuery = o.query.Encode()
	}

	upgrade, err := http.NewRequestWithContext(r.Context(),
		http.MethodGet, targetURL.String(), nil)
	if err != nil {
		msg := err.Error()
		if o.buildErrPrefix != "" {
			msg = o.buildErrPrefix + err.Error()
		}
		writeError(w, http.StatusInternalServerError, "internal", msg)
		return
	}

	targetConn, _, err := websocket.Dial(r.Context(), targetURL.String(),
		&websocket.DialOptions{
			HTTPHeader: upgrade.Header,
			HTTPClient: peer.NoKeepAliveHTTPClient(10 * time.Second),
		})
	if err != nil {
		if o.onDialErr != nil {
			o.onDialErr(err)
		}
		writeError(w, http.StatusBadGateway, "bad_gateway",
			o.dialErrPrefix+err.Error())
		return
	}
	defer targetConn.CloseNow()

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		if o.onAcceptErr != nil {
			o.onAcceptErr(err)
		}
		return
	}
	defer clientConn.CloseNow()
	clientConn.SetReadLimit(256 * 1024)
	targetConn.SetReadLimit(256 * 1024)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer cancel(); copyWS(ctx, clientConn, targetConn) }()
	go func() { defer wg.Done(); defer cancel(); copyWS(ctx, targetConn, clientConn) }()
	wg.Wait()

	if o.onClosed != nil {
		o.onClosed()
	}
}
