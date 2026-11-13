package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Session ids in the shape OpenCode 1.19.3 generates.
const (
	ocSID1 = "ses_eb96caae1ffeNQtLNqB50oU8p4"
	ocSID2 = "ses_eb9681278ffeTz386G0jH8fwgf"
	ocSID3 = "ses_eb96f397fffeYN24NtXaaOmKIU"
)

func collectGeminiEvents(t *testing.T, lines ...string) ([]ChatEvent, *geminiStreamResult) {
	t.Helper()
	r := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var events []ChatEvent
	res := parseGeminiStream(r, silentLogger(), func(e ChatEvent) bool {
		events = append(events, e)
		return true
	})
	return events, res
}

// Lines captured from `opencode run --format json --thinking` (1.19.3),
// trimmed to the fields kojo reads.
var ocSampleStream = []string{
	`{"type":"step_start","timestamp":1,"sessionID":"` + ocSID2 + `","part":{"id":"prt_a","type":"step-start"}}`,
	`{"type":"reasoning","timestamp":2,"sessionID":"` + ocSID2 + `","part":{"id":"prt_b","type":"reasoning","text":"**Plan**\n\nCall the tools."}}`,
	`{"type":"tool_use","timestamp":3,"sessionID":"` + ocSID2 + `","part":{"type":"tool","tool":"kojotest_secret_word","callID":"call1","state":{"status":"completed","input":{},"output":"PERIWINKLE-42"},"id":"prt_c"}}`,
	`{"type":"tool_use","timestamp":4,"sessionID":"` + ocSID2 + `","part":{"type":"tool","tool":"bash","callID":"call2","state":{"status":"completed","input":{"command":"echo \"$(date +%Y)\""},"output":"2026\n"},"id":"prt_d"}}`,
	`{"type":"step_finish","timestamp":5,"sessionID":"` + ocSID2 + `","part":{"id":"prt_e","reason":"tool-calls","type":"step-finish","tokens":{"total":9000,"input":8000,"output":20,"reasoning":100,"cache":{"write":0,"read":0}},"cost":0.02}}`,
	`not json noise`,
	`{"type":"step_start","timestamp":6,"sessionID":"` + ocSID2 + `","part":{"id":"prt_f","type":"step-start"}}`,
	`{"type":"text","timestamp":7,"sessionID":"` + ocSID2 + `","part":{"id":"prt_g","type":"text","text":"The word is PERIWINKLE-42."}}`,
	`{"type":"text","timestamp":7,"sessionID":"` + ocSID2 + `","part":{"id":"prt_g","type":"text","text":"The word is PERIWINKLE-42."}}`,
	`{"type":"text","timestamp":8,"sessionID":"` + ocSID2 + `","part":{"id":"prt_h","type":"text","text":"Year: 2026"}}`,
	`{"type":"step_finish","timestamp":9,"sessionID":"` + ocSID2 + `","part":{"id":"prt_i","reason":"stop","type":"step-finish","tokens":{"total":9100,"input":300,"output":30,"reasoning":50,"cache":{"write":10,"read":8000}},"cost":0.01}}`,
}

func TestParseGeminiStream_Sample(t *testing.T) {
	evs, res := collectGeminiEvents(t, ocSampleStream...)
	if res.sessionID != ocSID2 {
		t.Fatalf("sessionID = %q", res.sessionID)
	}
	if !res.finished || res.cancelled || res.streamError != "" {
		t.Fatalf("finished=%v cancelled=%v err=%q", res.finished, res.cancelled, res.streamError)
	}
	if res.text != "The word is PERIWINKLE-42.\n\nYear: 2026" {
		t.Fatalf("text = %q (duplicate part must be dropped)", res.text)
	}
	if res.thinking != "**Plan**\n\nCall the tools." {
		t.Fatalf("thinking = %q", res.thinking)
	}
	if len(res.toolUses) != 2 || res.toolUses[0].Name != "kojotest_secret_word" || res.toolUses[1].Name != "bash" {
		t.Fatalf("toolUses = %+v", res.toolUses)
	}
	if res.toolUses[1].Input != `{"command":"echo \"$(date +%Y)\""}` || res.toolUses[1].Output != "2026\n" {
		t.Fatalf("bash tool = %+v", res.toolUses[1])
	}
	if res.toolUses[0].Output != "PERIWINKLE-42" {
		t.Fatalf("mcp tool output = %q", res.toolUses[0].Output)
	}
	// Usage sums both steps: cache writes bill as input, reasoning as output.
	want := Usage{InputTokens: 8000 + 300 + 10, OutputTokens: 20 + 100 + 30 + 50, CacheReadInputTokens: 8000}
	if res.usage == nil || res.usage.InputTokens != want.InputTokens || res.usage.OutputTokens != want.OutputTokens || res.usage.CacheReadInputTokens != want.CacheReadInputTokens {
		t.Fatalf("usage = %+v, want %+v", res.usage, want)
	}
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	wantTypes := []string{"thinking", "tool_use", "tool_result", "tool_use", "tool_result", "text", "text"}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Fatalf("event types = %v, want %v", types, wantTypes)
	}
	if evs[6].Delta != "\n\nYear: 2026" {
		t.Fatalf("second text delta = %q", evs[6].Delta)
	}
}

func TestParseGeminiStream_ErrorsAndToolError(t *testing.T) {
	_, res := collectGeminiEvents(t,
		`{"type":"step_start","sessionID":"`+ocSID1+`","part":{"id":"p1"}}`,
		`{"type":"tool_use","sessionID":"`+ocSID1+`","part":{"id":"p2","tool":"read","callID":"c","state":{"status":"error","input":{"filePath":"/nope"},"error":"File not found"}}}`,
		`{"type":"error","sessionID":"`+ocSID1+`","error":{"name":"APIError","data":{"message":"Resource exhausted"}}}`,
	)
	if res.streamError != "APIError: Resource exhausted" {
		t.Fatalf("streamError = %q", res.streamError)
	}
	if len(res.toolUses) != 1 || res.toolUses[0].Output != "Error: File not found" {
		t.Fatalf("tool error = %+v", res.toolUses)
	}
	if res.finished {
		t.Fatal("no terminal step_finish was sent")
	}
	if got := classifyGeminiProcessError(nil, "", res); got != "APIError: Resource exhausted" {
		t.Fatalf("classify = %q", got)
	}
	_, res = collectGeminiEvents(t, `{"type":"error","error":{"name":"UnknownError","data":{"message":"boom"}}}`)
	if res.streamError != "boom" {
		t.Fatalf("UnknownError prefix kept: %q", res.streamError)
	}
}

func TestParseGeminiStream_RejectsMalformedSessionID(t *testing.T) {
	_, res := collectGeminiEvents(t, `{"type":"text","sessionID":"--help","part":{"id":"x","text":"hi"}}`)
	if res.sessionID != "" {
		t.Fatalf("accepted %q", res.sessionID)
	}
}

func TestParseGeminiStream_CancelStopsEarly(t *testing.T) {
	r := strings.NewReader(strings.Join(ocSampleStream, "\n"))
	n := 0
	res := parseGeminiStream(r, silentLogger(), func(ChatEvent) bool { n++; return n < 2 })
	if !res.cancelled || n != 2 {
		t.Fatalf("cancelled=%v n=%d", res.cancelled, n)
	}
}

func TestClassifyGeminiProcessError(t *testing.T) {
	res := &geminiStreamResult{}
	if got := classifyGeminiProcessError(nil, "\x1b[91mError: \x1b[0mModel not found: google/x", res); got != "opencode exited without a result: Error: Model not found: google/x" {
		t.Fatalf("no-result = %q", got)
	}
	if got := classifyGeminiProcessError(&exec.ExitError{}, "Error: Session not found", res); !strings.Contains(got, "Session not found") {
		t.Fatalf("exit = %q", got)
	}
	if !isGeminiStaleSessionError("\x1b[91mError: Session not found") {
		t.Fatal("stale session not detected")
	}
	if got := classifyGeminiProcessError(nil, "", &geminiStreamResult{finished: true}); got != "" {
		t.Fatalf("clean run = %q", got)
	}
}

func TestBuildGeminiArgs(t *testing.T) {
	got := buildGeminiArgs("gemini-4-argon", "high", ocSID1)
	want := []string{"run", "--format", "json", "--thinking", "--auto", "--title", openCodeChatTitle,
		"-m", "google/gemini-4-argon", "--variant", "high", "-s", ocSID1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q", got)
	}
	if got := buildGeminiArgs("other", "", ""); containsArg(got, "--variant") || containsArg(got, "-s") || !containsArg(got, "--thinking") {
		t.Fatalf("fresh args = %q", got)
	}
}

func TestGeminiModelAndVariant(t *testing.T) {
	for in, want := range map[string]string{"": GeminiDefaultModel, "flash": "gemini-3.8-flash", "gemini-3.8-flash": "gemini-3.8-flash", "gemini-4-argon": "gemini-4-argon", "gemini-4-carbon": "gemini-4-carbon", "gemini-4-hydrogen": "gemini-4-hydrogen", "custom-x": "custom-x"} {
		if got := resolveGeminiModel(in); got != want {
			t.Errorf("resolveGeminiModel(%q) = %q, want %q", in, got, want)
		}
	}
	cases := []struct{ model, effort, want string }{
		{"gemini-4-argon", "", "medium"}, // empty effort still pins a variant
		{"gemini-4-argon", "low", "low"},
		{"gemini-4-argon", "medium", "medium"},
		{"gemini-4-argon", "high", "high"},
		{"gemini-4-carbon", "low", "low"},
		{"gemini-4-carbon", "", "medium"},
		{"gemini-4-carbon", "xhigh", "medium"},
		{"gemini-4-hydrogen", "", "high"},
		{"gemini-4-hydrogen", "low", "low"},
		{"gemini-4-hydrogen", "xhigh", "xhigh"},
		{"gemini-4-hydrogen", "max", "high"},
		{"gemini-4-argon", "max", "medium"},
		{"flash", "low", "low"},
		{"", "high", "high"},
		{"gemini-3-flash-base", "high", ""},
	}
	for _, c := range cases {
		if got := geminiVariant(c.model, c.effort); got != c.want {
			t.Errorf("geminiVariant(%q, %q) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
}

func TestEffectiveGeminiMaxOutputTokens(t *testing.T) {
	for in, want := range map[int]int{-5: 65536, 0: 65536, 1: 1, 12345: 12345, 65536: 65536, 70000: 65536} {
		if got := EffectiveGeminiMaxOutputTokens(in); got != want {
			t.Errorf("EffectiveGeminiMaxOutputTokens(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestValidMaxOutputTokens(t *testing.T) {
	for v, want := range map[int]bool{-1: false, 0: true, 1: true, 8192: true, GeminiMaxOutputTokensLimit: true, GeminiMaxOutputTokensLimit + 1: false} {
		if got := ValidMaxOutputTokens(v); got != want {
			t.Fatalf("ValidMaxOutputTokens(%d) = %v", v, got)
		}
	}
}

func TestValidToolModelEffort_Gemini(t *testing.T) {
	cases := []struct {
		model, effort string
		ok            bool
	}{
		{"", "", true},
		{"", "medium", true},
		{"gemini-4-argon", "low", true},
		{"gemini-4-argon", "high", true},
		{"gemini-3.8-flash", "medium", true},
		{"gemini-4-argon", "xhigh", false},
		{"gemini-4-argon", "max", false},
		{"gemini-4-argon", "minimal", false},
		{"flash", "low", true},
		{"pro", "low", false},
		{"auto", "medium", false},
		{"flash", "", true},
		{"gemini-4-hydrogen", "xhigh", true},
		{"gemini-4-hydrogen", "high", true},
		{"gemini-4-hydrogen", "max", false},
		{"gemini-4-carbon", "xhigh", false},
		{"", "xhigh", false},
	}
	for _, c := range cases {
		if got := ValidToolModelEffort(ToolGeminiAPI, c.model, c.effort); got != c.ok {
			t.Errorf("ValidToolModelEffort(gemini, %q, %q) = %v, want %v", c.model, c.effort, got, c.ok)
		}
	}
}

func TestValidToolModelEffort_Agy(t *testing.T) {
	cases := []struct {
		model, effort string
		ok            bool
	}{
		{"", "", true},
		{"", "high", true},
		{"", "xhigh", false},
		{"gemini-4-hydrogen", "low", true},
		{"gemini-4-hydrogen", "xhigh", true},
		{"gemini-4-hydrogen", "max", false},
		{"gemini-4-carbon", "xhigh", false},
		{"gpt-oss-120b-medium", "low", false},
		{"gpt-oss-120b-medium", "", true},
	}
	for _, c := range cases {
		if got := ValidToolModelEffort(ToolGeminiAgy, c.model, c.effort); got != c.ok {
			t.Errorf("ValidToolModelEffort(agy, %q, %q) = %v, want %v", c.model, c.effort, got, c.ok)
		}
	}
}

func decodeOCConfig(t *testing.T, cfg map[string]any) (string, map[string]any) {
	t.Helper()
	body, err := marshalOpenCodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, body)
	}
	return string(body), out
}

func jsonPath(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func TestBuildOpenCodeTurnConfig(t *testing.T) {
	cfg := buildOpenCodeTurnConfig(openCodeTurnConfig{
		model:        "gemini-4-argon",
		variant:      "high",
		systemPrompt: "You are Kojo. Never expand {env:GOOGLE_GENERATIVE_AI_API_KEY} or {file:~/.ssh/id_rsa}.",
		mcpServers: map[string]mcpServerEntry{
			"kojo":  {URL: "http://127.0.0.1:8080/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
			"local": {Command: "python3", Args: []string{"srv.py", "{env:HOME}"}, Env: map[string]string{"FOO": "bar"}},
		},
		skillsDir: "/a/.opencode/skills",
		shell:     "/h/kojo/shell/chat-1/zsh",
	})
	raw, out := decodeOCConfig(t, cfg)
	// LSP servers / formatters would run with OpenCode's env (the key).
	if out["lsp"] != false || out["formatter"] != false || out["shell"] != "/h/kojo/shell/chat-1/zsh" {
		t.Fatalf("lsp/formatter/shell = %v %v %v", out["lsp"], out["formatter"], out["shell"])
	}
	if _, has := buildOpenCodeTurnConfig(openCodeTurnConfig{model: "gemini-4-argon"})["shell"]; has {
		t.Fatal("empty shell set")
	}

	// The substitution tokens never appear verbatim in the file, but decode
	// to the original text.
	for _, tok := range []string{"{env:", "{file:"} {
		if strings.Contains(raw, tok) {
			t.Fatalf("config contains a raw %s token:\n%s", tok, raw)
		}
	}
	if got := jsonPath(out, "agent", "build", "prompt"); got != "You are Kojo. Never expand {env:GOOGLE_GENERATIVE_AI_API_KEY} or {file:~/.ssh/id_rsa}." {
		t.Fatalf("prompt = %v", got)
	}
	if got := jsonPath(out, "agent", "build", "variant"); got != "high" {
		t.Fatalf("variant = %v", got)
	}
	for _, k := range []string{"model", "small_model"} {
		if out[k] != "google/gemini-4-argon" {
			t.Fatalf("%s = %v", k, out[k])
		}
	}
	model := jsonPath(out, "provider", "google", "models", "gemini-4-argon")
	if got := jsonPath(model, "limit", "output"); got != float64(65536) {
		t.Fatalf("limit.output = %v (0 must mean 65536)", got)
	}
	for _, lvl := range []string{"low", "medium", "high"} {
		tc := jsonPath(model, "variants", lvl, "thinkingConfig")
		if jsonPath(tc, "thinkingLevel") != lvl || jsonPath(tc, "includeThoughts") != true {
			t.Fatalf("variant %s = %v", lvl, tc)
		}
	}
	if got := jsonPath(out, "skills", "paths"); !reflect.DeepEqual(got, []any{"/a/.opencode/skills"}) {
		t.Fatalf("skills = %v", got)
	}
	remote := jsonPath(out, "mcp", "kojo")
	if jsonPath(remote, "type") != "remote" || jsonPath(remote, "url") != "http://127.0.0.1:8080/mcp" || jsonPath(remote, "oauth") != false || jsonPath(remote, "headers", "Authorization") != "Bearer t" {
		t.Fatalf("remote mcp = %v", remote)
	}
	local := jsonPath(out, "mcp", "local")
	if jsonPath(local, "type") != "local" || !reflect.DeepEqual(jsonPath(local, "command"), []any{"python3", "srv.py", "{env:HOME}"}) {
		t.Fatalf("local mcp = %v", local)
	}
	env, _ := jsonPath(local, "environment").(map[string]any)
	if env["FOO"] != "bar" || env["GOOGLE_GENERATIVE_AI_API_KEY"] != "" || env["OPENCODE_CONFIG"] != "" {
		t.Fatalf("local mcp env = %v", env)
	}
	if _, ok := env["XDG_CONFIG_HOME"]; !ok {
		t.Fatalf("local mcp env does not restore XDG_CONFIG_HOME: %v", env)
	}
	// Every isolation-only variable is blanked for the MCP child.
	for _, k := range []string{"OPENCODE_TEST_HOME", "OPENCODE_TEST_MANAGED_CONFIG_DIR", "OPENCODE_DISABLE_AUTOUPDATE",
		"OPENCODE_DISABLE_CHANNEL_DB", "OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX", "KOJO_OC_ORIG_XDG_CONFIG_HOME", openCodeStreamEnv} {
		if v, ok := env[k]; !ok || v != "" {
			t.Errorf("local mcp env %s = %v (present %v), want blank", k, v, ok)
		}
	}

	// gemini-4-hydrogen also gets the xhigh variant; the others stop at high.
	if jsonPath(model, "variants", "xhigh") != nil {
		t.Fatalf("gemini-4-argon has an xhigh variant")
	}
	_, out = decodeOCConfig(t, buildOpenCodeTurnConfig(openCodeTurnConfig{model: "gemini-4-hydrogen", variant: "xhigh"}))
	model = jsonPath(out, "provider", "google", "models", "gemini-4-hydrogen")
	for _, lvl := range []string{"low", "medium", "high", "xhigh"} {
		if tc := jsonPath(model, "variants", lvl, "thinkingConfig"); jsonPath(tc, "thinkingLevel") != lvl {
			t.Fatalf("hydrogen variant %s = %v", lvl, tc)
		}
	}
	if got := jsonPath(out, "agent", "build", "variant"); got != "xhigh" {
		t.Fatalf("hydrogen variant = %v", got)
	}

	// Custom maxOutputTokens and an undescribed model: no variants, no prompt.
	_, out = decodeOCConfig(t, buildOpenCodeTurnConfig(openCodeTurnConfig{model: "gemini-x", maxOutputTokens: 12345}))
	model = jsonPath(out, "provider", "google", "models", "gemini-x")
	if jsonPath(model, "limit", "output") != float64(12345) || jsonPath(model, "variants") != nil {
		t.Fatalf("custom model = %v", model)
	}
	if jsonPath(out, "agent", "build", "prompt") != nil || jsonPath(out, "agent", "build", "variant") != nil || out["mcp"] != nil || out["skills"] != nil {
		t.Fatalf("unexpected keys: %v", out)
	}
}

func TestOpenCodeGlobalConfig(t *testing.T) {
	_, out := decodeOCConfig(t, openCodeGlobalConfig())
	if out["autoupdate"] != false || out["share"] != "disabled" || out["snapshot"] != false ||
		out["lsp"] != false || out["formatter"] != false {
		t.Fatalf("global config = %v", out)
	}
	if !reflect.DeepEqual(out["enabled_providers"], []any{"google"}) {
		t.Fatalf("providers = %v", out["enabled_providers"])
	}
	if jsonPath(out, "permission", "*") != "allow" || jsonPath(out, "permission", "question") != "deny" {
		t.Fatalf("permission = %v", out["permission"])
	}
}

func TestEnsureOpenCodeHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".opencode-home")
	if err := ensureOpenCodeHome(home); err != nil {
		t.Fatal(err)
	}
	cfgDir := openCodeConfigDir(home)
	for _, f := range []string{"opencode.json", "package.json", "package-lock.json", ".gitignore", filepath.Join("plugin", "kojo-env.js")} {
		if _, err := os.Stat(filepath.Join(cfgDir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(cfgDir, "node_modules")); err != nil || !fi.IsDir() {
		t.Fatalf("node_modules not seeded: %v", err)
	}
	// Idempotent and does not rewrite an unchanged file.
	p := filepath.Join(cfgDir, "opencode.json")
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, past, past)
	if err := ensureOpenCodeHome(home); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); !fi.ModTime().Equal(past) {
		t.Fatal("unchanged config rewritten")
	}

	// A planted symlink is refused.
	if runtime.GOOS != "windows" {
		evil := filepath.Join(t.TempDir(), ".opencode-home")
		if err := os.MkdirAll(evil, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := t.TempDir()
		if err := os.Symlink(victim, filepath.Join(evil, "config")); err != nil {
			t.Fatal(err)
		}
		if err := ensureOpenCodeHome(evil); err == nil {
			t.Fatal("symlinked config dir accepted")
		}
		if entries, _ := os.ReadDir(victim); len(entries) != 0 {
			t.Fatalf("wrote through symlink: %v", entries)
		}
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v // last wins, as for exec
	}
	return m
}

func TestGeminiCommandEnv_Isolates(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"model":"evil"}`)
	t.Setenv("OPENCODE_PERMISSION", "{}")
	t.Setenv("GOOGLE_API_KEY", "other")
	t.Setenv("GEMINI_API_KEY", "other2")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "1")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://x")
	t.Setenv("KOJO_GEMINI_API_KEY", "kojo-env-key")
	t.Setenv("XDG_CONFIG_HOME", "/operator/config")
	t.Setenv("XDG_DATA_HOME", "")
	os.Unsetenv("XDG_STATE_HOME")
	t.Setenv("PWD", "/kojo/launch/dir")
	dir := t.TempDir()
	env := geminiCommandEnv("ag_x", dir, "the-key", "/cfg.json", 12345)
	m := envMap(env)
	// opencode run prefers $PWD over the real cwd for its directory.
	pwds := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PWD=") {
			pwds++
		}
	}
	if pwds != 1 || m["PWD"] != dir {
		t.Errorf("PWD entries = %d, PWD = %q, want one %q", pwds, m["PWD"], dir)
	}
	if aux := envMap(openCodeAuxEnv("ag_x", dir)); aux["PWD"] != dir {
		t.Errorf("aux PWD = %q, want %q", aux["PWD"], dir)
	}
	for _, k := range []string{"OPENCODE_CONFIG_CONTENT", "OPENCODE_PERMISSION", "GOOGLE_API_KEY", "GEMINI_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "OTEL_EXPORTER_OTLP_ENDPOINT", "KOJO_GEMINI_API_KEY"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s leaked into the child env", k)
		}
	}
	home := openCodeHome(dir)
	want := map[string]string{
		"XDG_CONFIG_HOME":                        filepath.Join(home, "config"),
		"XDG_DATA_HOME":                          filepath.Join(home, "data"),
		"XDG_STATE_HOME":                         filepath.Join(home, "state"),
		"XDG_CACHE_HOME":                         filepath.Join(home, "cache"),
		"OPENCODE_TEST_HOME":                     home,
		"OPENCODE_DISABLE_AUTOUPDATE":            "1",
		"OPENCODE_DISABLE_PROJECT_CONFIG":        "1",
		"OPENCODE_DISABLE_DEFAULT_PLUGINS":       "1",
		"OPENCODE_DISABLE_MODELS_FETCH":          "1",
		"OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX": "12345",
		"OPENCODE_CONFIG":                        "/cfg.json",
		"GOOGLE_GENERATIVE_AI_API_KEY":           "the-key",
		"KOJO_OC_ORIG_XDG_CONFIG_HOME":           "/operator/config",
		"KOJO_OC_ORIG_XDG_DATA_HOME":             "",
		"KOJO_OC_ORIG_XDG_STATE_HOME":            openCodeUnset,
		"KOJO_AGENT_ID":                          "ag_x",
		openCodeStreamEnv:                        "1",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	// Each isolation var appears once (no inherited duplicate).
	seen := map[string]int{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		seen[k]++
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "OPENCODE_CONFIG", "GOOGLE_GENERATIVE_AI_API_KEY", openCodeStreamEnv} {
		if seen[k] != 1 {
			t.Errorf("%s appears %d times", k, seen[k])
		}
	}
	// Default output cap.
	if m := envMap(geminiCommandEnv("ag_x", dir, "", "", 0)); m["OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX"] != "65536" {
		t.Fatalf("default cap = %q", m["OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX"])
	}
}

func TestGeminiSessionRefs(t *testing.T) {
	dir := t.TempDir()
	writeGeminiSessionIDFor(dir, "", ocSID1, silentLogger())
	writeGeminiSessionIDFor(dir, "slack:1", ocSID2, silentLogger())
	writeGeminiSessionIDFor(dir, "bad", "--help", silentLogger())
	if got := readGeminiSessionIDFor(dir, ""); got != ocSID1 {
		t.Fatalf("main = %q", got)
	}
	if got := readGeminiSessionIDFor(dir, "slack:1"); got != ocSID2 {
		t.Fatalf("keyed = %q", got)
	}
	if got := readGeminiSessionIDFor(dir, "bad"); got != "" {
		t.Fatalf("malformed persisted: %q", got)
	}
	if !geminiSessionIDPersisted(dir, ocSID2) || geminiSessionIDPersisted(dir, ocSID3) {
		t.Fatal("geminiSessionIDPersisted")
	}
	// A tampered ref is dropped on read.
	if err := os.WriteFile(geminiSessionIDFile(dir), []byte("-s; rm -rf /"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != "" {
		t.Fatalf("tampered ref = %q", got)
	}
	if _, err := os.Stat(geminiSessionIDFile(dir)); !os.IsNotExist(err) {
		t.Fatal("tampered ref kept")
	}
}

// plantOpenCodeDB creates a minimal OpenCode session table holding ids.
func plantOpenCodeDB(t *testing.T, home string, rows ...[3]string) {
	t.Helper()
	if err := os.MkdirAll(openCodeDataDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", openCodeDBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT NOT NULL, title TEXT NOT NULL, time_updated INTEGER NOT NULL, time_archived INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := db.Exec(`INSERT OR REPLACE INTO session (id, directory, title, time_updated) VALUES (?, ?, ?, ?)`, r[0], r[1], r[2], i+1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenCodeSessionDBQueries(t *testing.T) {
	home := t.TempDir()
	if exists, known := openCodeSessionExists(home, ocSID1); exists || !known {
		t.Fatalf("no db: exists=%v known=%v", exists, known)
	}
	work := t.TempDir()
	plantOpenCodeDB(t, home,
		[3]string{ocSID1, work, "tui session"},
		[3]string{ocSID2, work, openCodeChatTitle},
		[3]string{ocSID3, "/elsewhere", "other dir"},
	)
	if exists, known := openCodeSessionExists(home, ocSID1); !exists || !known {
		t.Fatalf("exists=%v known=%v", exists, known)
	}
	if exists, _ := openCodeSessionExists(home, "ses_000000000000AAAAAAAAAAAAAA"); exists {
		t.Fatal("missing session reported")
	}
	// The newest kojo-chat session in workDir is skipped.
	if got := openCodeLatestInteractiveSession(home, work); got != ocSID1 {
		t.Fatalf("latest interactive = %q", got)
	}
	if got := openCodeLatestInteractiveSession(home, t.TempDir()); got != "" {
		t.Fatalf("other dir = %q", got)
	}
}

// fakeOpenCodeScript stands in for `opencode`: it logs argv, the env vars
// kojo sets, stdin and the turn config, then emits a canned stream.
const fakeOpenCodeScript = `#!/bin/sh
log="$FAKE_OC_LOG"
case "$1" in
session|export|import) echo "AUX $*" >> "$log"; [ "$1" = import ] && echo "Imported session"
	if [ "$1 $2" = "session delete" ] && [ -z "$FAKE_OC_NODELETE" ] && command -v sqlite3 >/dev/null; then
		sqlite3 "$XDG_DATA_HOME/opencode/opencode.db" "DELETE FROM session WHERE id='$3'" 2>/dev/null
	fi
	exit 0;;
esac
{
  echo "ARGS $*"
  echo "KEY=${GOOGLE_GENERATIVE_AI_API_KEY}"
  echo "GAK=${GOOGLE_API_KEY-unset}"
  echo "MAX=${OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX}"
  echo "XDG=${XDG_DATA_HOME}"
  echo "STDIN<<"; cat; echo ">>"
} >> "$log"
cp "$OPENCODE_CONFIG" "$log.cfg"
if [ -n "$FAKE_OC_STALE" ]; then
  case " $* " in *" -s "*) echo "Error: Session not found" >&2; exit 1;; esac
fi
sid="$FAKE_OC_SID"
echo "{\"type\":\"step_start\",\"sessionID\":\"$sid\",\"part\":{\"id\":\"p1\"}}"
echo "{\"type\":\"reasoning\",\"sessionID\":\"$sid\",\"part\":{\"id\":\"p2\",\"text\":\"thinking\"}}"
echo "{\"type\":\"text\",\"sessionID\":\"$sid\",\"part\":{\"id\":\"p3\",\"text\":\"hello\"}}"
echo "{\"type\":\"step_finish\",\"sessionID\":\"$sid\",\"part\":{\"id\":\"p4\",\"reason\":\"stop\",\"tokens\":{\"input\":1000,\"output\":10,\"reasoning\":5,\"cache\":{\"read\":2000,\"write\":0}}}}"
`

func setupFakeOpenCode(t *testing.T) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sh fake")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(fakeOpenCodeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logPath = filepath.Join(t.TempDir(), "log")
	t.Setenv("FAKE_OC_LOG", logPath)
	t.Setenv("FAKE_OC_STALE", "")
	t.Setenv("FAKE_OC_NODELETE", "")
	t.Setenv("GEMINI_API_KEY", "test-key")
	t.Setenv("GOOGLE_API_KEY", "should-be-stripped")
	t.Setenv("KOJO_GEMINI_API_KEY", "")
	return logPath
}

func runGeminiTurn(t *testing.T, b *GeminiBackend, a *Agent, msg string, opts ChatOptions) []ChatEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ch, err := b.Chat(ctx, a, msg, "SYSTEM PROMPT", opts)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var evs []ChatEvent
	for e := range ch {
		evs = append(evs, e)
	}
	if len(evs) == 0 {
		t.Fatalf("no events (ctx err: %v)", ctx.Err())
	}
	return evs
}

func lastDone(t *testing.T, evs []ChatEvent) ChatEvent {
	t.Helper()
	e := evs[len(evs)-1]
	if e.Type != "done" {
		t.Fatalf("last event = %+v", e)
	}
	return e
}

func TestGeminiChat_PersistsAndResumes(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	origNow := geminiNow
	geminiNow = func() time.Time { return time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { geminiNow = origNow })

	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_oc_chat", Effort: "high", MaxOutputTokens: 12345}
	dir := agentDir(a.ID)
	t.Setenv("FAKE_OC_SID", ocSID1)

	evs := runGeminiTurn(t, b, a, "first", ChatOptions{})
	done := lastDone(t, evs)
	if done.ErrorMessage != "" || done.Message == nil || done.Message.Content != "hello" {
		t.Fatalf("done = %+v", done)
	}
	if done.Usage == nil || done.Usage.InputTokens != 1000 || done.Usage.OutputTokens != 15 || done.Usage.CacheReadInputTokens != 2000 {
		t.Fatalf("usage = %+v", done.Usage)
	}
	wantCost := (1000*2.0 + 2000*0.10 + 15*10.0) / 1e6
	if math.Abs(done.Usage.CostUSD-wantCost) > 1e-12 {
		t.Fatalf("cost = %v, want %v", done.Usage.CostUSD, wantCost)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != ocSID1 {
		t.Fatalf("ref = %q", got)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	for _, want := range []string{
		"ARGS run --format json --thinking --auto --title kojo-chat -m google/gemini-4-argon --variant high\n",
		"KEY=test-key\n", "GAK=unset\n", "MAX=12345\n",
		"XDG=" + filepath.Join(openCodeHome(dir), "data") + "\n",
		"STDIN<<\nfirst",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("log missing %q:\n%s", want, s)
		}
	}
	cfgRaw, err := os.ReadFile(logPath + ".cfg")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatal(err)
	}
	if jsonPath(cfg, "agent", "build", "prompt") != "SYSTEM PROMPT" || jsonPath(cfg, "provider", "google", "models", "gemini-4-argon", "limit", "output") != float64(12345) {
		t.Fatalf("turn config = %s", cfgRaw)
	}
	// The per-turn config is removed after the turn.
	if m, _ := filepath.Glob(filepath.Join(geminiKojoDir(dir), "turn-*.json")); len(m) != 0 {
		t.Fatalf("turn config left behind: %v", m)
	}
	if strings.Contains(string(cfgRaw), "test-key") {
		t.Fatal("API key written into the config")
	}

	// Second turn resumes the stored session (the DB holds it).
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{ocSID1, dir, openCodeChatTitle})
	_ = os.Remove(logPath)
	a.Effort = ""
	a.MaxOutputTokens = 0
	lastDone(t, runGeminiTurn(t, b, a, "second", ChatOptions{}))
	log, _ = os.ReadFile(logPath)
	if !strings.Contains(string(log), "--variant medium -s "+ocSID1+"\n") || !strings.Contains(string(log), "MAX=65536\n") {
		t.Fatalf("resume log:\n%s", log)
	}

	// OneShot never resumes and never touches the stored ref; its own
	// session is deleted afterwards.
	_ = os.Remove(logPath)
	t.Setenv("FAKE_OC_SID", ocSID3)
	lastDone(t, runGeminiTurn(t, b, a, "oneshot", ChatOptions{OneShot: true}))
	log, _ = os.ReadFile(logPath)
	if strings.Contains(string(log), " -s ") || !strings.Contains(string(log), "AUX session delete "+ocSID3) {
		t.Fatalf("oneshot log:\n%s", log)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != ocSID1 {
		t.Fatalf("oneshot moved the ref: %q", got)
	}

	// A keyed thread gets its own ref.
	lastDone(t, runGeminiTurn(t, b, a, "thread", ChatOptions{SessionKey: "k1"}))
	if got := readGeminiSessionIDFor(dir, "k1"); got != ocSID3 {
		t.Fatalf("thread ref = %q", got)
	}
}

func TestGeminiChat_DropsRefMissingFromDB(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_oc_stale"}
	dir := agentDir(a.ID)
	writeGeminiSessionIDFor(dir, "", ocSID1, silentLogger())
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{ocSID3, dir, "x"})
	t.Setenv("FAKE_OC_SID", ocSID2)
	lastDone(t, runGeminiTurn(t, b, a, "hi", ChatOptions{}))
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), " -s ") {
		t.Fatalf("resumed a session the DB does not hold:\n%s", log)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != ocSID2 {
		t.Fatalf("ref = %q", got)
	}
}

// A session stored under another directory (e.g. an agent dir that moved)
// must not be resumed: `opencode run -s` would hang with no output.
func TestGeminiChat_DropsRefFromOtherDirectory(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_oc_otherdir"}
	dir := agentDir(a.ID)
	writeGeminiSessionIDFor(dir, "", ocSID1, silentLogger())
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{ocSID1, "/somewhere/else", openCodeChatTitle})
	t.Setenv("FAKE_OC_SID", ocSID2)
	lastDone(t, runGeminiTurn(t, b, a, "hi", ChatOptions{}))
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), " -s ") {
		t.Fatalf("resumed a session from another directory:\n%s", log)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != ocSID2 {
		t.Fatalf("ref = %q", got)
	}
}

func TestOpenCodeSessionDirMismatch(t *testing.T) {
	home := t.TempDir()
	if bad, _ := openCodeSessionDirMismatch(home, ocSID1, "/x"); bad {
		t.Fatal("no db must not be a mismatch")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	plantOpenCodeDB(t, home,
		[3]string{ocSID1, real, "a"},
		[3]string{ocSID2, "/elsewhere", "b"},
	)
	if bad, _ := openCodeSessionDirMismatch(home, ocSID1, real); bad {
		t.Error("same dir reported as mismatch")
	}
	if bad, _ := openCodeSessionDirMismatch(home, ocSID1, link); bad {
		t.Error("symlinked dir reported as mismatch")
	}
	if bad, got := openCodeSessionDirMismatch(home, ocSID2, real); !bad || got != "/elsewhere" {
		t.Errorf("other dir: bad=%v got=%q", bad, got)
	}
	if bad, _ := openCodeSessionDirMismatch(home, ocSID3, real); bad {
		t.Error("missing row reported as mismatch")
	}
}

func TestGeminiChat_RetriesFreshOnSessionNotFound(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_oc_retry"}
	dir := agentDir(a.ID)
	writeGeminiSessionIDFor(dir, "", ocSID1, silentLogger())
	// Make the DB unreadable as a session table so the pre-check is
	// inconclusive and OpenCode itself reports the stale id.
	if err := os.MkdirAll(openCodeDataDir(openCodeHome(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openCodeDBPath(openCodeHome(dir)), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OC_SID", ocSID2)
	t.Setenv("FAKE_OC_STALE", "1")
	done := lastDone(t, runGeminiTurn(t, b, a, "hi", ChatOptions{}))
	if done.ErrorMessage != "" {
		t.Fatalf("done err = %q", done.ErrorMessage)
	}
	log, _ := os.ReadFile(logPath)
	if n := strings.Count(string(log), "ARGS run"); n != 2 {
		t.Fatalf("runs = %d:\n%s", n, log)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != ocSID2 {
		t.Fatalf("ref = %q", got)
	}
}

func TestGeminiChat_NoAPIKey(t *testing.T) {
	setupFakeOpenCode(t)
	for _, k := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "KOJO_GEMINI_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY"} {
		t.Setenv(k, "")
	}
	b := NewGeminiBackend(silentLogger(), nil)
	_, err := b.Chat(context.Background(), &Agent{ID: "ag_oc_nokey"}, "hi", "", ChatOptions{})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err = %v", err)
	}
}

// TestGeminiChat_CancelKillsTree checks that cancelling a turn kills the
// commands OpenCode started in their own process groups.
func TestGeminiChat_CancelKillsTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh fake")
	}
	setupFakeOpenCode(t)
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := "#!/bin/sh\n" +
		"set -m\n" + // job control: the child gets its own process group, like OpenCode's bash tool
		"sleep 300 &\n" +
		"echo $! > " + shSingleQuote(pidFile) + "\n" +
		"echo '{\"type\":\"step_start\",\"sessionID\":\"" + ocSID1 + "\",\"part\":{\"id\":\"p\"}}'\n" +
		"wait\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := NewGeminiBackend(silentLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Chat(ctx, &Agent{ID: "ag_oc_cancel"}, "hi", "", ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var pid string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(pidFile); len(strings.TrimSpace(string(b))) > 0 {
			pid = strings.TrimSpace(string(b))
			break
		}
	}
	if pid == "" {
		t.Fatal("child never started")
	}
	cancel()
	for range ch {
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if exec.Command("kill", "-0", pid).Run() != nil {
			return
		}
	}
	_ = exec.Command("kill", "-9", pid).Run()
	t.Fatalf("detached child %s survived cancel", pid)
}

// A turn aborted while OpenCode runs a command: a background job an
// earlier (already exited) shell-tool shell left behind is outside the
// process tree but in that shell's logged group, and is reaped too. The
// stream here ends by EOF (no event after the abort), not by a failed
// send.
func TestGeminiChat_CancelReapsBackgroundJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh fake")
	}
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl (setsid) unavailable")
	}
	setupFakeOpenCode(t)
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "pid")
	// Like OpenCode's shell tool: the configured shell, started setsid.
	script := "#!/bin/sh\n" +
		"sh=$(sed -n 's/.*\"shell\": *\"\\([^\"]*\\)\".*/\\1/p' \"$OPENCODE_CONFIG\")\n" +
		"[ -x \"$sh\" ] || { echo noshell >&2; exit 3; }\n" +
		"perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' \"$sh\" -c 'sleep 300 >/dev/null 2>&1 & echo $! > " + pidFile + "'\n" +
		"echo '{\"type\":\"step_start\",\"sessionID\":\"" + ocSID1 + "\",\"part\":{\"id\":\"p\"}}'\n" +
		"sleep 300\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHELL", "/bin/sh")
	b := NewGeminiBackend(silentLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Chat(ctx, &Agent{ID: "ag_oc_reap"}, "hi", "", ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && pid == 0; time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(pidFile); len(strings.TrimSpace(string(b))) > 0 {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if pid <= 1 {
		t.Fatal("background job never started")
	}
	t.Cleanup(func() { _ = exec.Command("kill", "-9", strconv.Itoa(pid)).Run() })
	time.Sleep(200 * time.Millisecond) // let its shell exit (the job is orphaned)
	cancel()
	for range ch {
	}
	if exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil {
		t.Fatalf("background job %d survived the abort", pid)
	}
	if entries, _ := os.ReadDir(openCodeShellRoot(openCodeHome(agentDir("ag_oc_reap")))); len(entries) != 0 {
		t.Fatalf("shell dirs left: %v", entries)
	}
}

// TestGeminiChat_Live drives the real opencode binary against the Gemini
// API. Opt-in: KOJO_GEMINI_LIVE=1 with `opencode` on PATH and
// GEMINI_API_KEY set (KOJO_GEMINI_LIVE_MODEL overrides the model).
func TestGeminiChat_Live(t *testing.T) {
	if os.Getenv("KOJO_GEMINI_LIVE") != "1" {
		t.Skip("set KOJO_GEMINI_LIVE=1 to run")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_oc_live", Model: os.Getenv("KOJO_GEMINI_LIVE_MODEL"), Effort: "low"}
	evs := runGeminiTurn(t, b, a, "Run `echo \"$(expr 6 \\* 7)\"` with your bash tool and tell me the output.", ChatOptions{})
	done := lastDone(t, evs)
	if done.ErrorMessage != "" || done.Usage == nil || done.Usage.OutputTokens == 0 {
		t.Fatalf("done = %+v usage=%+v", done, done.Usage)
	}
	if !strings.Contains(done.Message.Content, "42") {
		t.Fatalf("reply = %q", done.Message.Content)
	}
	evs = runGeminiTurn(t, b, a, "What number did the command print? Answer with the number only.", ChatOptions{})
	done = lastDone(t, evs)
	if !strings.Contains(done.Message.Content, "42") {
		t.Fatalf("resume reply = %q", done.Message.Content)
	}
}

// A planted .opencode-home/data link must not make a session clear delete
// the link target's OpenCode DB.
func TestClearGeminiSessionCounted_RefusesSymlinkedData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	withGeminiTransferEnv(t)
	id := "ag_oc_clear_link"
	dir := agentDir(id)
	home := openCodeHome(dir)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(victim), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("operator db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}
	writeGeminiSessionIDFor(dir, "", ocSID1, silentLogger())
	if _, err := clearGeminiSessionCounted(id); err == nil {
		t.Fatal("clear through a symlinked data dir succeeded")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("link target removed: %v", err)
	}

	// With a real data dir the clear works and removes DB + refs.
	_ = os.Remove(filepath.Join(home, "data"))
	db := openCodeDBPath(home)
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := clearGeminiSessionCounted(id)
	if err != nil || n != 1 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("db not removed: %v", err)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != "" {
		t.Fatalf("ref survived: %q", got)
	}
}

func TestNewOpenCodeSessionID(t *testing.T) {
	// A session OpenCode 1.19.3 created at 1792190689973 got the id
	// ses_eb91c4d4affe… (Identifier.descending, counter 1).
	at := time.UnixMilli(1792190689973)
	id, err := newOpenCodeSessionID(at)
	if err != nil {
		t.Fatal(err)
	}
	if !isGeminiSessionID(id) || !strings.HasPrefix(id, "ses_eb91c4d4affe") {
		t.Fatalf("id = %q", id)
	}
	later, _ := newOpenCodeSessionID(at.Add(time.Millisecond))
	if !(later < id) {
		t.Fatalf("descending order broken: %q !< %q", later, id)
	}
	again, _ := newOpenCodeSessionID(at)
	if again == id {
		t.Fatal("random tail repeated")
	}
}

func TestRunOpenCodeIn_CapsStdoutAndKillsTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh fake")
	}
	bin := t.TempDir()
	oc := filepath.Join(bin, "opencode")
	// Writes far past the cap, then would hang if not killed.
	script := "#!/bin/sh\nhead -c 300000 /dev/zero | tr '\\0' 'x'\nexec sleep 60\n"
	if err := os.WriteFile(oc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	start := time.Now()
	out, err := runOpenCodeIn(context.Background(), oc, "ag_x", home, t.TempDir(), 1000, "export", ocSID1)
	var big errGeminiOversized
	if !errors.As(err, &big) || out != nil || big.size <= 1000 {
		t.Fatalf("out=%d err=%v", len(out), err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("oversized export was not killed promptly (%v)", d)
	}

	// Under the cap the output is returned whole.
	if err := os.WriteFile(oc, []byte("#!/bin/sh\nprintf hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = runOpenCodeIn(context.Background(), oc, "ag_x", home, t.TempDir(), 1000, "export", ocSID1)
	if err != nil || string(out) != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestCreateOpenCodeTUISession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh fake")
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("needs the sqlite3 CLI for the fake opencode")
	}
	bin := t.TempDir()
	oc := filepath.Join(bin, "opencode")
	logf := filepath.Join(t.TempDir(), "log")
	// Fake `opencode import <file>`: records cwd/PWD/argv and the file,
	// then stores the id like OpenCode would.
	script := `#!/bin/sh
{ echo "ARGV:$*"; echo "PWD:$PWD"; echo "CWD:$(pwd -P)"; cat "$2"; echo; } > ` + shSingleQuote(logf) + `
id=$(sed -n 's/.*"id":"\(ses_[^"]*\)".*/\1/p' "$2")
d=$(printf %s "$PWD" | sed "s/'/''/g")
mkdir -p "$XDG_DATA_HOME/opencode"
sqlite3 "$XDG_DATA_HOME/opencode/opencode.db" "CREATE TABLE IF NOT EXISTS session (id TEXT PRIMARY KEY, directory TEXT, title TEXT, time_updated INTEGER); INSERT INTO session VALUES ('$id', '$d', 'x', 1);"
echo "Imported session: $id"
`
	if err := os.WriteFile(oc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	work := filepath.Join(t.TempDir(), "work 'dir'")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", "/stale")
	id, err := createOpenCodeTUISession(context.Background(), oc, "ag_x", home, work)
	if err != nil {
		t.Fatal(err)
	}
	if !isGeminiSessionID(id) {
		t.Fatalf("id = %q", id)
	}
	if exists, known := openCodeSessionExists(home, id); !exists || !known {
		t.Fatal("session not in the db")
	}
	logb, _ := os.ReadFile(logf)
	log := string(logb)
	realWork, _ := filepath.EvalSymlinks(work)
	if !strings.Contains(log, "ARGV:import "+openCodeKojoDir(home)+"/.tui-session-") || !strings.Contains(log, "\nPWD:"+work+"\n") || !strings.Contains(log, "\nCWD:"+realWork+"\n") {
		t.Fatalf("import invocation:\n%s", log)
	}
	var exp struct {
		Info struct {
			ID, Directory, Title, Slug, ProjectID, Version string
			Time                                           struct{ Created, Updated int64 }
		}
		Messages []any
	}
	body := log[strings.Index(log, "{"):]
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &exp); err != nil {
		t.Fatalf("export body %q: %v", body, err)
	}
	if exp.Info.ID != id || exp.Info.Directory != work || !strings.HasPrefix(exp.Info.Title, "New session - ") ||
		exp.Info.Slug == "" || exp.Info.ProjectID == "" || exp.Info.Version == "" || exp.Info.Time.Created == 0 || exp.Messages == nil || len(exp.Messages) != 0 {
		t.Fatalf("export = %+v", exp)
	}
	if left, _ := filepath.Glob(filepath.Join(openCodeKojoDir(home), ".tui-session-*")); len(left) != 0 {
		t.Fatalf("temp export left behind: %v", left)
	}

	// An import that does not land is an error (the launch then falls back).
	if err := os.WriteFile(oc, []byte("#!/bin/sh\necho nope\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := createOpenCodeTUISession(context.Background(), oc, "ag_x", home, work); err == nil {
		t.Fatal("expected an error")
	}
}

// A data dir linked out of the home is never opened (it could be the
// operator's own OpenCode data) and the transfer checks refuse it.
func TestOpenCodeDB_RefusesSymlinkedDataDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	withGeminiTransferEnv(t)
	dir := agentDir("ag_oc_dblink")
	home := openCodeHome(dir)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "opencode", "opencode.db"), []byte("operator db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}
	if db, err := openOpenCodeDB(home); err == nil || db != nil {
		t.Fatalf("opened a DB through a symlinked data dir (err=%v)", err)
	}
	if _, known := openCodeSessionExists(home, ocSID1); known {
		t.Fatal("session lookup through a symlinked data dir reported known")
	}
	if err := geminiCheckRealDirs(dir); err == nil {
		t.Fatal("transfer check accepted a symlinked data dir")
	}
}

func TestSweepGeminiPTYLaunchDirs_RefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	withGeminiTransferEnv(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "oc-0123.sh")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(victim, old, old)
	if err := os.MkdirAll(openCodeKojoDir(geminiPlainHome()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, geminiPTYLaunchDir()); err != nil {
		t.Fatal(err)
	}
	sweepGeminiPTYLaunchDirs(time.Now())
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("sweep followed the link: %v", err)
	}
	// A real launch dir is swept.
	_ = os.Remove(geminiPTYLaunchDir())
	if err := os.MkdirAll(geminiPTYLaunchDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(geminiPTYLaunchDir(), "oc-0123.sh")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(stale, old, old)
	sweepGeminiPTYLaunchDirs(time.Now())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale wrapper kept: %v", err)
	}
}

func TestHasEnvPrefix(t *testing.T) {
	if !hasEnvPrefix("OPENCODE_CONFIG=x", "OPENCODE") || hasEnvPrefix("PWDX=1", "PWD=") {
		t.Fatal("exact match broken")
	}
	got := hasEnvPrefix("opencode_config_content={}", "OPENCODE")
	if want := runtime.GOOS == "windows"; got != want {
		t.Fatalf("lower-case name match = %v, want %v on %s", got, want, runtime.GOOS)
	}
}

// A link planted on a file inside the home (opencode.db, a WAL sidecar,
// prompt history...) stops the launch; links inside node_modules do not.
func TestEnsureOpenCodeHome_RefusesFileLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	withGeminiTransferEnv(t)
	outside := filepath.Join(t.TempDir(), "operator.db")
	if err := os.WriteFile(outside, []byte("operator db"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"opencode.db", "opencode.db-wal"} {
		home := openCodeHome(agentDir("ag_oc_filelink_" + strings.TrimPrefix(rel, "opencode.db")))
		if err := ensureOpenCodeHome(home); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(openCodeDataDir(home), rel)
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := ensureOpenCodeHome(home); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("%s: ensureOpenCodeHome accepted a planted link (err=%v)", rel, err)
		}
	}
	home := openCodeHome(agentDir("ag_oc_nmlink"))
	if err := ensureOpenCodeHome(home); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(openCodeConfigDir(home), "node_modules", "x")); err != nil {
		t.Fatal(err)
	}
	if err := ensureOpenCodeHome(home); err == nil || !strings.Contains(err.Error(), "not managed by kojo") {
		t.Fatalf("node_modules entry accepted (err=%v)", err)
	}
}

// Anything OpenCode would load as code or config that kojo did not write
// (extra global configs, plugins, custom tools, ~/.opencode, the managed
// dir, auth.json, node_modules packages) fails the launch, and removing
// it makes the home usable again.
func TestEnsureOpenCodeHome_RefusesUnmanaged(t *testing.T) {
	withGeminiTransferEnv(t)
	home := openCodeHome(agentDir("ag_oc_unmanaged"))
	if err := ensureOpenCodeHome(home); err != nil {
		t.Fatal(err)
	}
	cfgDir := openCodeConfigDir(home)
	planted := append(openCodeUnmanagedPaths(home),
		filepath.Join(cfgDir, "plugin", "evil.js"),
		filepath.Join(cfgDir, "plugin", "evil.ts"),
		filepath.Join(cfgDir, "node_modules", "@opencode-ai"),
	)
	for _, p := range planted {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := ensureOpenCodeHome(home)
		if err == nil || !strings.Contains(err.Error(), "not managed by kojo") || !strings.Contains(err.Error(), p) {
			t.Fatalf("%s: planted file accepted (err=%v)", p, err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := ensureOpenCodeHome(home); err != nil {
			t.Fatalf("%s removed but home still refused: %v", p, err)
		}
	}
	// A planted directory is refused too, not only a file.
	if err := os.MkdirAll(filepath.Join(home, ".opencode", "plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureOpenCodeHome(home); err == nil || !strings.Contains(err.Error(), "not managed by kojo") {
		t.Fatalf("planted ~/.opencode dir accepted (err=%v)", err)
	}
}

// SQLite sidecar links are refused by the DB open and by the transfer
// check, not only the opencode.db link itself.
func TestOpenCodeDB_RefusesSidecarLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	withGeminiTransferEnv(t)
	dir := agentDir("ag_oc_sidecar")
	home := openCodeHome(dir)
	if err := os.MkdirAll(openCodeDataDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openCodeDBPath(home), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "wal")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, openCodeDBPath(home)+"-wal"); err != nil {
		t.Fatal(err)
	}
	if db, err := openOpenCodeDB(home); err == nil || db != nil {
		t.Fatalf("opened a DB with a linked WAL (err=%v)", err)
	}
	if err := geminiCheckRealDirs(dir); err == nil {
		t.Fatal("transfer check accepted a linked WAL")
	}
}

func TestNoteOpenCodeImportFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), ocSID1+".json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < openCodeImportMaxAttempts; i++ {
		noteOpenCodeImportFailure(path)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("attempt %d: export parked too early: %v", i, err)
		}
	}
	noteOpenCodeImportFailure(path)
	if _, err := os.Stat(path + ".failed"); err != nil {
		t.Fatalf("export not parked after %d attempts: %v", openCodeImportMaxAttempts, err)
	}
	if _, err := os.Stat(path + openCodeImportAttemptsSuffix); !os.IsNotExist(err) {
		t.Fatalf("attempt counter left behind: %v", err)
	}
}

// While a device-switch export for the stored session is still pending
// (its import was postponed), a turn neither resumes the stale local copy
// nor drops the ref; once the import lands the next turn proceeds.
func TestGeminiChat_PendingImportBlocksResume(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("fake session delete needs sqlite3")
	}
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_oc_pending"}
	dir := agentDir(a.ID)
	writeGeminiSessionIDFor(dir, "", ocSID1, silentLogger())
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{ocSID1, dir, openCodeChatTitle})
	pending := filepath.Join(geminiPendingImportDir(dir), ocSID1+".json")
	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte(`{"info":{"id":"`+ocSID1+`"},"messages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OC_SID", ocSID1)
	t.Setenv("FAKE_OC_NODELETE", "1") // dropping the local copy fails: import postponed

	if _, err := b.Chat(context.Background(), a, "hi", "SYS", ChatOptions{}); err == nil || !strings.Contains(err.Error(), "not imported yet") {
		t.Fatalf("Chat err = %v, want a not-imported-yet error", err)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "ARGS run") {
		t.Fatalf("ran a turn while the import was pending:\n%s", log)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != ocSID1 {
		t.Fatalf("ref = %q, want it kept", got)
	}
	if !hasPendingOpenCodeImport(dir, ocSID1) {
		t.Fatal("pending export gone")
	}

	t.Setenv("FAKE_OC_NODELETE", "")
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{ocSID1, dir, openCodeChatTitle})
	lastDone(t, runGeminiTurn(t, b, a, "again", ChatOptions{}))
	if hasPendingOpenCodeImport(dir, ocSID1) {
		t.Fatal("export still pending after a successful import")
	}
}

// ResetData's clearGeminiHome removes the OpenCode home and the legacy
// gemini-cli home, unlinking (never following) a planted symlink.
func TestClearGeminiHome_RemovesLegacyHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(os.Getenv("HOME"), ".config"))
	const id = "ag_oc_reset"
	dir := agentDir(id)
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(openCodeHome(dir), "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(geminiLegacyCLIHome(dir), ".gemini", "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	clearGeminiHome(id, silentLogger())
	for _, p := range []string{openCodeHome(dir), geminiLegacyCLIHome(dir)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the reset (%v)", p, err)
		}
	}
	if err := os.Symlink(outside, geminiLegacyCLIHome(dir)); err != nil {
		t.Fatal(err)
	}
	clearGeminiHome(id, silentLogger())
	if _, err := os.Lstat(geminiLegacyCLIHome(dir)); !os.IsNotExist(err) {
		t.Fatalf("symlink survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("followed the symlink: %v", err)
	}
}

// A turn whose shell wrapper cannot be written (here: a file where the
// shell root dir belongs) fails instead of running with unreapable
// background jobs. (A planted symlink is refused even earlier, by
// ensureOpenCodeHome.)
func TestGeminiChat_ShellWrapperFailureFailsTurn(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_oc_shlink"}
	home := openCodeHome(agentDir(a.ID))
	if err := ensureOpenCodeHome(home); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openCodeShellRoot(home), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OC_SID", ocSID1)
	if _, err := b.Chat(context.Background(), a, "hi", "SYS", ChatOptions{}); err == nil || !strings.Contains(err.Error(), "shell wrapper") {
		t.Fatalf("Chat err = %v", err)
	}
	if log, _ := os.ReadFile(logPath); strings.Contains(string(log), "ARGS run") {
		t.Fatalf("ran without the wrapper:\n%s", log)
	}
}

// sameOpenCodeDir lets the file system decide: links and (on a
// case-insensitive volume) case variants are the same dir, a sibling that
// only differs in case on a case-sensitive volume is not.
func TestSameOpenCodeDir(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "Agent")
	if err := os.Mkdir(a, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if !sameOpenCodeDir(a, a+string(filepath.Separator)) || !sameOpenCodeDir("", a) {
		t.Fatal("identical dirs differ")
	}
	if sameOpenCodeDir(a, other) {
		t.Fatal("different dirs are the same")
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "link")
		if err := os.Symlink(a, link); err != nil {
			t.Fatal(err)
		}
		if !sameOpenCodeDir(a, link) {
			t.Fatal("a link to the dir differs")
		}
	}
	lower := filepath.Join(root, "agent")
	if _, err := os.Stat(lower); err == nil {
		// Case-insensitive volume: the case variant is the same dir.
		if !sameOpenCodeDir(a, lower) {
			t.Fatal("case variant on a case-insensitive volume differs")
		}
	} else {
		if err := os.Mkdir(lower, 0o700); err != nil {
			t.Fatal(err)
		}
		if sameOpenCodeDir(a, lower) {
			t.Fatal("case-sensitive siblings are the same")
		}
	}
	if sameOpenCodeDir(filepath.Join(root, "gone"), a) {
		t.Fatal("a missing stored dir matches")
	}
}
