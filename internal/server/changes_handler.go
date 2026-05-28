package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/loppo-llc/kojo/internal/store"
)

// handleChanges serves `GET /api/v1/changes?since=<seq>[&table=<name>][&limit=<n>]`,
// the catch-up cursor for peers whose WebSocket invalidation feed
// dropped frames (overflow eviction, brief disconnect, etc).
//
// Response body shape:
//
//	{
//	  "events": [
//	    {"seq": N, "table": "...", "id": "...", "etag": "...", "op": "insert|update|delete", "ts": <millis>},
//	    ...
//	  ],
//	  "next_since": <seq to pass on the next poll>,
//	  "watermark":  <smallest seq currently retained — informational>,
//	  "truncated":  <currently always false; reserved for when a
//	                retention worker prunes events the caller never saw>
//	}
//
// Note: `truncated` is reserved for a future retention slice. Today
// the events table grows without bound, so the cursor cannot lose
// events out from under a peer. When retention lands it will set a
// persisted "pruned-through" floor in kv; the handler will then flip
// `truncated` true when `since < floor`.
//
// `since` defaults to 0; `limit` clamps to [1, 5000] (default 500);
// `table` is optional and narrows to one domain.
//
// Auth: gated by OwnerOnlyMiddleware on the public listener; not on the
// AllowNonOwner allowlist for the agent listener — same rationale as
// /api/v1/events.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	st := s.changeStore()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "change cursor not configured")
		return
	}

	q := r.URL.Query()
	since, err := parseSinceParam(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limit, err := parseLimitParam(q.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	res, err := st.ListEventsSince(r.Context(), since, store.ListEventsSinceOptions{
		Table: q.Get("table"),
		Limit: limit,
	})
	if err != nil {
		s.logger.Error("changes: ListEventsSince failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "failed to read events")
		return
	}

	type wireEvent struct {
		Seq   int64  `json:"seq"`
		Table string `json:"table"`
		ID    string `json:"id"`
		ETag  string `json:"etag"`
		Op    string `json:"op"`
		TS    int64  `json:"ts"`
	}
	out := struct {
		Events    []wireEvent `json:"events"`
		NextSince int64       `json:"next_since"`
		Watermark int64       `json:"watermark"`
		Truncated bool        `json:"truncated"`
	}{
		Events:    make([]wireEvent, 0, len(res.Events)),
		NextSince: res.NextSince,
		Watermark: res.Watermark,
		// Truncation reporting requires a persisted "pruned-through"
		// floor — the simple `since < MIN(seq)` check trips for every
		// fresh peer because seq is epoch-ms-based and even an empty-
		// retention DB has MIN(seq) ~= now. Until a background
		// retention worker (and matching kv row) lands, we cannot
		// distinguish "you missed events" from "you have never polled
		// before", so we leave Truncated=false. Watermark is still
		// reported so a peer can see the oldest seq the Hub still has.
		Truncated: false,
	}
	for _, e := range res.Events {
		out.Events = append(out.Events, wireEvent{
			Seq:   e.Seq,
			Table: e.Table,
			ID:    e.ID,
			ETag:  e.ETag,
			Op:    string(e.Op),
			TS:    e.TS,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.logger.Warn("changes: encode failed", "err", err)
	}
}

// changeStore returns the Store backing the cursor read. We pull it
// from the agent manager (the only place that already holds a Store
// reference); if no agent manager is configured the endpoint stays
// disabled — there are no events to read anyway.
func (s *Server) changeStore() *store.Store {
	if s.agents == nil {
		return nil
	}
	return s.agents.Store()
}

// parseSinceParam: empty/missing = 0; negative or non-numeric is a
// 400 — we don't silently coerce because a typoed cursor that
// degrades to since=0 would re-deliver the entire history.
func parseSinceParam(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, errInvalidSince
	}
	if n < 0 {
		return 0, errInvalidSince
	}
	return n, nil
}

// parseLimitParam: empty = default (handled in store), else clamp to
// [1, 5000]. Out-of-range is a 400 rather than silent clamp so an
// over-eager client learns to back off.
func parseLimitParam(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errInvalidLimit
	}
	if n < 1 || n > 5000 {
		return 0, errInvalidLimit
	}
	return n, nil
}

// errInvalidSince / errInvalidLimit are surfaced to the caller as 400
// bodies. Keeping them as package vars lets tests assert on them
// without depending on the exact phrasing.
var (
	errInvalidSince = httpClientErr("since must be a non-negative integer")
	errInvalidLimit = httpClientErr("limit must be an integer in [1, 5000]")
)

type httpClientErr string

func (e httpClientErr) Error() string { return string(e) }
