package agent

// SkippedSessionFile records one session file the §3.7 device-switch
// transfer left behind (oversized, unreadable ref, missing rollout,
// …). Previously a bare filename in a warn log; the structured shape
// rides the agent-sync wire and the switch response so the loss is
// visible to the agent (arrival prompt), the operator (switch API
// response), and the owner (UI notice).
//
// JSON tags are camelCase so the same struct can be stored verbatim
// under the agent's settings_json (lastTransferSkips) and consumed by
// the web UI without a mapping layer.
type SkippedSessionFile struct {
	// Path is the file name / relative path inside the session
	// store (e.g. "b7d2….jsonl", "terminal/call-1.log").
	Path string `json:"path"`
	// Reason is a stable, short slug: "oversized", "unreadable",
	// "invalid_ref_name", "unreadable_ref", "rollout_path_unknown",
	// "rollout_path_invalid", "rollout_missing".
	Reason string `json:"reason"`
	// SizeBytes is the on-disk size when known (0 otherwise).
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// skippedFilePaths flattens a skip list into bare paths for compact
// log lines.
func skippedFilePaths(skips []SkippedSessionFile) []string {
	if len(skips) == 0 {
		return nil
	}
	out := make([]string, 0, len(skips))
	for _, s := range skips {
		out = append(out, s.Path)
	}
	return out
}

// ArrivalNotes carries per-switch caveats into the arrival prompt so
// the just-arrived agent knows what state did NOT make the trip:
//
//   - TokenReissued: target lacked the raw $KOJO_AGENT_TOKEN and
//     auto-re-issued it at finalize (no manual re-issue needed).
//   - DegradedFlushes: the switch ran in degraded mode; the listed
//     source-side flushes ("memory_flush", "persona_flush") were
//     skipped, so memory / persona may be stale.
//   - TransferSkips: session files dropped from the sync payload.
type ArrivalNotes struct {
	TokenReissued   bool
	DegradedFlushes []string
	TransferSkips   []SkippedSessionFile
}
