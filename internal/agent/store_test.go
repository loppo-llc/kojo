package agent

import "testing"

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
