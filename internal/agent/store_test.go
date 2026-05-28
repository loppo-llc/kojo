package agent

import (
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestAgentToSettings_StripsLegacyCronMessage verifies that a prior
// settings_json containing the legacy `cronMessage` key does NOT
// survive a Save once the in-memory Agent has been migrated (CronMessage
// cleared). Without `cronMessage` in reservedAgentKeys the forward-compat
// merge loop would copy the stale value back into the new settings map
// because omitempty drops the field from json.Marshal(a), so the prior
// key would look "unknown" and resurrect on every Save.
func TestAgentToSettings_StripsLegacyCronMessage(t *testing.T) {
	a := &Agent{ID: "ag", Name: "n"} // CronMessage left empty (migrated state)
	prior := map[string]any{
		"cronMessage": "legacy body the migration already consumed",
		// A genuinely unknown key the binary doesn't model — this one
		// MUST survive the merge so the forward-compat guarantee still
		// holds.
		"futureField": "keep me",
	}
	got, err := agentToSettings(a, prior)
	if err != nil {
		t.Fatalf("agentToSettings: %v", err)
	}
	if _, present := got["cronMessage"]; present {
		t.Errorf("cronMessage leaked back into settings_json: %v", got["cronMessage"])
	}
	if got["futureField"] != "keep me" {
		t.Errorf("forward-compat merge dropped futureField: %v", got["futureField"])
	}
}

// TestAgentToSettings_ClearedModeledFieldsDoNotResurrect locks down the
// silent-rollback fix: when a user clears a field this binary models
// (Silent Hours toggle off, custom base URL removed, Slack bot disabled,
// etc.), json.Marshal drops the field via `omitempty` / nil pointer. The
// forward-compat merge used to treat that as "prior wins" and copy the
// stale value back into the new settings map — making the clear invisible
// in the DB and surfacing as a rollback the next time the row was read
// from disk (e.g. after kojo-switch-device removed the agent from the
// source's in-memory map and the UI fell back to ListRemote / GetRemote).
//
// modeledAgentKeys now skips every Agent-declared JSON key in the merge,
// so a cleared field stays cleared. The futureField check at the end
// re-asserts the forward-compat guarantee for keys the binary doesn't
// model.
func TestAgentToSettings_ClearedModeledFieldsDoNotResurrect(t *testing.T) {
	a := &Agent{ID: "ag", Name: "n"} // every optional field left empty/nil
	prior := map[string]any{
		// String fields with `omitempty`
		"silentStart":   "22:00",
		"silentEnd":     "07:00",
		"workDir":       "/old/path",
		"customBaseURL": "http://legacy.example",
		"thinkingMode":  "on",
		// Pointer fields (omitempty triggers on nil)
		"notifyDuringSilent":  true,
		"deviceSwitchEnabled": false,
		// Nested-struct pointer fields cleared to nil — these
		// pinpoint the slackBot/tts disable-then-resurrect path that
		// the codex review explicitly called out. prior holds the
		// full nested map shape the JSON round-trip would otherwise
		// preserve.
		"slackBot": map[string]any{
			"enabled":        true,
			"threadReplies":  false,
			"respondDM":      true,
			"respondMention": true,
			"respondThread":  false,
		},
		"tts": map[string]any{
			"voice": "ja-JP-Wavenet-A",
			"model": "tts-1",
		},
		// Slice fields
		"allowedTools":        []any{"Edit", "Read"},
		"allowProtectedPaths": []any{"claude"},
		// Numeric with omitempty (zero drops out)
		"resumeIdleMinutes": float64(15),
		// Case-variant prior key — encoding/json is case-insensitive
		// on Unmarshal, so a hand-edited / future-binary row could
		// ship "SilentStart" and the modeled-key guard must still
		// refuse to resurrect it. Pins down the canonical-lower
		// lookup in agentToSettings.
		"SilentStart": "20:00",
		// A genuinely-unknown future key MUST survive.
		"futureField": "keep me",
	}
	got, err := agentToSettings(a, prior)
	if err != nil {
		t.Fatalf("agentToSettings: %v", err)
	}
	cleared := []string{
		"silentStart", "silentEnd", "workDir", "customBaseURL", "thinkingMode",
		"notifyDuringSilent", "deviceSwitchEnabled",
		"slackBot", "tts",
		"allowedTools", "allowProtectedPaths", "resumeIdleMinutes",
		// Case-variant must also be filtered.
		"SilentStart",
	}
	for _, k := range cleared {
		if v, present := got[k]; present {
			t.Errorf("cleared modeled key %q resurrected from prior: %v", k, v)
		}
	}
	if got["futureField"] != "keep me" {
		t.Errorf("forward-compat merge dropped futureField: %v", got["futureField"])
	}
}

// TestAgentToSettings_SetModeledFieldsPersist confirms the fix doesn't
// regress the non-cleared path: when the in-memory Agent carries a value,
// agentToSettings must propagate it to the result map regardless of what
// prior held.
func TestAgentToSettings_SetModeledFieldsPersist(t *testing.T) {
	notifyOff := false
	a := &Agent{
		ID:                 "ag",
		Name:               "n",
		SilentStart:        "23:00",
		SilentEnd:          "06:00",
		NotifyDuringSilent: &notifyOff,
		CustomBaseURL:      "http://new.example",
	}
	prior := map[string]any{
		"silentStart":        "22:00",
		"silentEnd":          "07:00",
		"notifyDuringSilent": true,
		"customBaseURL":      "http://legacy.example",
	}
	got, err := agentToSettings(a, prior)
	if err != nil {
		t.Fatalf("agentToSettings: %v", err)
	}
	if got["silentStart"] != "23:00" {
		t.Errorf("silentStart not propagated: got %v", got["silentStart"])
	}
	if got["silentEnd"] != "06:00" {
		t.Errorf("silentEnd not propagated: got %v", got["silentEnd"])
	}
	if got["notifyDuringSilent"] != false {
		t.Errorf("notifyDuringSilent not propagated: got %v", got["notifyDuringSilent"])
	}
	if got["customBaseURL"] != "http://new.example" {
		t.Errorf("customBaseURL not propagated: got %v", got["customBaseURL"])
	}
}

// TestModeledAgentKeys_CoversSilentHoursAndFriends pins down the static
// set so future Agent struct edits that accidentally drop a JSON tag (or
// rename a field without keeping the JSON name stable) trip this test
// rather than silently regressing the rollback bug.
func TestModeledAgentKeys_CoversSilentHoursAndFriends(t *testing.T) {
	required := []string{
		"silentStart", "silentEnd", "notifyDuringSilent",
		"workDir", "customBaseURL", "thinkingMode",
		"allowedTools", "allowProtectedPaths",
		"resumeIdleMinutes", "deviceSwitchEnabled",
		"slackBot", "tts",
		"cronExpr", "timeoutMinutes",
	}
	for _, k := range required {
		// modeledAgentKeys is canonical-lower so the prior-merge
		// lookup matches case-variant rows; the required list is
		// written in the natural camelCase JSON shape and lowered
		// here so future edits to that list don't have to remember
		// the lookup convention.
		if !modeledAgentKeys[strings.ToLower(k)] {
			t.Errorf("modeledAgentKeys missing %q; clear-then-resurrect bug would re-emerge for this field", k)
		}
	}
}

// TestAgentToSettings_ReservedOnlyKeysDoNotResurrect locks down the
// reserved-keys-via-canonical-lower lookup: `notifySources` is reserved
// only (not modeled on Agent), so a prior row carrying it MUST NOT
// survive a Save. Also covers a case-variant ("NotifySources") to pin
// the lower-cased reserved set introduced alongside the modeled-key
// fix — without it the prior-merge ToLower lookup would silently
// permit reserved-only keys back into settings_json.
func TestAgentToSettings_ReservedOnlyKeysDoNotResurrect(t *testing.T) {
	a := &Agent{ID: "ag", Name: "n"}
	prior := map[string]any{
		"notifySources": map[string]any{"gmail": map[string]any{"enabled": true}},
		"NotifySources": "legacy stringified form",
		"intervalMinutes": float64(30), // legacy reserved key
		"futureField":     "keep me",
	}
	got, err := agentToSettings(a, prior)
	if err != nil {
		t.Fatalf("agentToSettings: %v", err)
	}
	for _, k := range []string{"notifySources", "NotifySources", "intervalMinutes"} {
		if _, present := got[k]; present {
			t.Errorf("reserved-only key %q resurrected from prior", k)
		}
	}
	if got["futureField"] != "keep me" {
		t.Errorf("forward-compat merge dropped futureField: %v", got["futureField"])
	}
}

// TestSettingsToAgent_StripsCaseVariantHolderPeer locks down the
// canonical-lower lookup in settingsToAgent: a settings_json row that
// carries a runtime-only HolderPeer key with non-canonical casing
// (e.g. "HolderPeer") must still be stripped before the JSON unmarshal
// into Agent. encoding/json's case-insensitive field matching would
// otherwise reflect the stale value back into a.HolderPeer, surfacing
// the wrong holder peer in the UI.
func TestSettingsToAgent_StripsCaseVariantHolderPeer(t *testing.T) {
	rec := &store.AgentRecord{
		ID:   "ag",
		Name: "n",
		Settings: map[string]any{
			"HolderPeer":  "peer-1",
			"holderPeer":  "peer-2", // canonical form also covered
			"persona":     "should be dropped from settings_json",
			"silentStart": "22:00", // legitimate field; must survive
		},
	}
	out := &Agent{}
	if err := settingsToAgent(rec, out); err != nil {
		t.Fatalf("settingsToAgent: %v", err)
	}
	if out.HolderPeer != "" {
		t.Errorf("HolderPeer leaked through case-variant strip: %q", out.HolderPeer)
	}
	if out.Persona != "" {
		// settingsToAgent docs explicitly leave Persona zero — caller
		// fills it from the join. Strip path covers the same key.
		t.Errorf("Persona leaked through settings_json strip: %q", out.Persona)
	}
	if out.SilentStart != "22:00" {
		t.Errorf("SilentStart not propagated through load: %q", out.SilentStart)
	}
}
