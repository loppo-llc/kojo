package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
	"github.com/loppo-llc/kojo/internal/proctree"
)

// The "gemini" tool runs Google Gemini models through OpenCode
// (github.com/anomalyco/opencode, `opencode`). The kojo tool id, the
// agent fields and the model ids stay "gemini"; only the CLI behind them
// is OpenCode. Every OpenCode-specific behaviour relied on below was
// verified against opencode 1.19.3.

// GeminiDefaultModel is the model kojo passes when the agent has none set.
// It is always passed explicitly (never left to the CLI default) so the
// per-turn provider config has a concrete model to describe.
const GeminiDefaultModel = "gemini-4-argon"

// geminiEffortModels maps the Gemini models whose thinking level kojo can
// drive through agent.Effort (low/medium/high → OpenCode variant →
// thinkingConfig.thinkingLevel) to the concrete id OpenCode is given.
// "flash" is the legacy gemini-cli alias kept for agents created before
// the switch; OpenCode has no aliases, so it is resolved here.
var geminiEffortModels = map[string]string{
	"gemini-4-carbon":  "gemini-4-carbon",
	"gemini-4-argon":   "gemini-4-argon",
	"gemini-3.8-flash": "gemini-3.8-flash",
	"flash":            "gemini-3.8-flash",
}

// geminiModelNames are the display names written into the provider config
// for the models kojo fully describes.
var geminiModelNames = map[string]string{
	"gemini-4-carbon":  "Gemini 4 Carbon",
	"gemini-4-argon":   "Gemini 4 Argon",
	"gemini-3.8-flash": "Gemini 3.8 Flash",
}

// geminiSessionIDPattern matches OpenCode session ids ("ses_" + 12 hex
// timestamp digits + 14 base62 characters). Every value read back from a
// ref file, the stream or the session DB is validated before it reaches
// argv (`-s`): the agent can write to its own directory, so an unvalidated
// ref could inject flags.
var geminiSessionIDPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func isGeminiSessionID(s string) bool { return geminiSessionIDPattern.MatchString(s) }

// openCodeChatTitle is the --title of every session kojo's chat backend
// creates. It skips OpenCode's title-generation request and lets the PTY
// launcher tell chat sessions apart from interactive ones.
const openCodeChatTitle = "kojo-chat"

// openCodeUnset marks an XDG variable that was unset in kojo's own
// environment (KOJO_OC_ORIG_* carries it to the env-restoring plugin).
const openCodeUnset = "__kojo_unset__"

// openCodeStreamEnv=1 (chat turns only) makes kojo's plugin mirror the
// root session's text/reasoning deltas, tool starts and subagent step
// usage to stdout as extra JSON lines (see parseGeminiStream): `opencode
// run --format json` itself prints only completed parts.
const openCodeStreamEnv = "KOJO_OC_STREAM"

var openCodeXDGVars = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"}

// geminiStripEnvPrefixes are removed from the inherited environment before
// the per-turn values are appended.
//   - OPENCODE* covers every OpenCode knob a parent could carry
//     (OPENCODE_CONFIG / _CONFIG_CONTENT / _CONFIG_DIR would inject config,
//     OPENCODE_DB / OPENCODE_AUTH_CONTENT would redirect state or auth,
//     OPENCODE_PERMISSION would override kojo's permissions, OPENCODE=1 /
//     OPENCODE_PID mark "running inside opencode").
//   - XDG_*_HOME are replaced with the per-agent dirs.
//   - The GOOGLE_* / GEMINI_* entries would switch the google provider to a
//     different key, base URL or auth method (Vertex, ADC). (filterEnv
//     itself drops every KOJO_* variable, incl. KOJO_GEMINI_API_KEY.)
//   - OTEL_EXPORTER_OTLP_* would turn on OpenCode's OTLP telemetry export.
//   - BUN_* tunes the runtime OpenCode is compiled with.
//   - PWD: `opencode run` takes its directory from $PWD before the real
//     cwd, so kojo's own PWD would put the session (and every tool call)
//     in kojo's launch directory; it is re-set to the child's dir.
var geminiStripEnvPrefixes = []string{
	"OPENCODE",
	"PWD=",
	"XDG_CONFIG_HOME=",
	"XDG_DATA_HOME=",
	"XDG_STATE_HOME=",
	"XDG_CACHE_HOME=",
	"GEMINI_",
	"GOOGLE_API_KEY=",
	"GOOGLE_GENERATIVE_AI_",
	"GOOGLE_GEMINI_",
	"GOOGLE_GENAI_",
	"GOOGLE_VERTEX_",
	"GOOGLE_CLOUD_ACCESS_TOKEN=",
	"GOOGLE_CLOUD_PROJECT=",
	"GOOGLE_CLOUD_LOCATION=",
	"GOOGLE_APPLICATION_CREDENTIALS=",
	"OTEL_EXPORTER_OTLP_",
	"BUN_",
	"AGENT_BROWSER_SESSION",
	"AGENT_BROWSER_COOKIE_DIR",
}

// GeminiBackend implements ChatBackend for Gemini models through OpenCode.
// It runs `opencode run --format json` headlessly with the prompt on stdin
// and parses the NDJSON event stream.
//
// Isolation: every agent gets its own OpenCode home
// (<agentDir>/.opencode-home) holding XDG config/data/state/cache dirs, so
// the operator's ~/.config/opencode, ~/.local/share/opencode (incl.
// auth.json) and ~/.opencode are never read or written. The API key only
// travels in the child's environment (GOOGLE_GENERATIVE_AI_API_KEY). The
// static global config (autoupdate/share off, allow-all permissions) is
// rewritten when it differs; everything per-turn (model, variant, system
// prompt, output limit, MCP servers) goes into a per-turn OPENCODE_CONFIG
// file that is removed when the turn ends.
//
// Sessions: the stream carries the OpenCode session id on every event;
// kojo stores it per ChatOptions.SessionKey and replays it with
// `-s <id>` on the next turn.
//
// The same backend also serves ToolCustomOpenCode (tool field): the turn
// config then defines a "kojo-<preset>" provider on
// @ai-sdk/openai-compatible (bundled in the opencode binary, so nothing is
// installed) pointed at the agent's whitelisted preset, with the agent's
// custom API key in KOJO_OC_CUSTOM_API_KEY.
type GeminiBackend struct {
	logger *slog.Logger
	creds  *CredentialStore
	tool   string // ToolGeminiAPI or ToolCustomOpenCode
}

func NewGeminiBackend(logger *slog.Logger, creds *CredentialStore) *GeminiBackend {
	return &GeminiBackend{logger: logger, creds: creds, tool: ToolGeminiAPI}
}

// NewCustomOpenCodeBackend returns the OpenCode backend for the
// custom-opencode tool.
func NewCustomOpenCodeBackend(logger *slog.Logger, creds *CredentialStore) *GeminiBackend {
	return &GeminiBackend{logger: logger, creds: creds, tool: ToolCustomOpenCode}
}

func (b *GeminiBackend) Name() string {
	if b.tool == "" {
		return ToolGeminiAPI
	}
	return b.tool
}

func (b *GeminiBackend) isCustom() bool { return b.tool == ToolCustomOpenCode }

func (b *GeminiBackend) Available() bool {
	_, err := exec.LookPath("opencode")
	return err == nil
}

// openCodeHome is the per-agent OpenCode home. Living inside the agent
// dir means ResetData's RemoveAll wipes it with everything else.
func openCodeHome(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".opencode-home")
}

// openCodeKojoDir holds kojo-owned state inside the OpenCode home
// (session refs, per-turn config files, pending device-switch imports);
// OpenCode itself only touches the XDG subdirs.
func openCodeKojoDir(home string) string { return filepath.Join(home, "kojo") }

func openCodeConfigDir(home string) string {
	return filepath.Join(home, "config", "opencode")
}

func openCodeDataDir(home string) string { return filepath.Join(home, "data", "opencode") }

// openCodeDBPath is OpenCode's session database. OPENCODE_DISABLE_CHANNEL_DB
// pins the name to opencode.db for every release channel.
func openCodeDBPath(home string) string { return filepath.Join(openCodeDataDir(home), "opencode.db") }

func geminiKojoDir(agentDirPath string) string { return openCodeKojoDir(openCodeHome(agentDirPath)) }

func geminiSessionIDFile(agentDirPath string) string {
	return filepath.Join(geminiKojoDir(agentDirPath), "session_id")
}

func geminiThreadRefDir(agentDirPath string) string {
	return filepath.Join(geminiKojoDir(agentDirPath), "threads")
}

// geminiPendingImportDir holds OpenCode session exports staged by a device
// switch; they are imported into the agent's DB before its next turn.
func geminiPendingImportDir(agentDirPath string) string {
	return filepath.Join(geminiKojoDir(agentDirPath), "pending-import")
}

// geminiSessionRefPath maps a SessionKey to its ref file. The key is
// hashed so a caller-supplied value can't steer the path.
func geminiSessionRefPath(agentDirPath, sessionKey string) string {
	if sessionKey == "" {
		return geminiSessionIDFile(agentDirPath)
	}
	return filepath.Join(geminiThreadRefDir(agentDirPath), "key-"+agentIDToUUID(sessionKey))
}

func readGeminiSessionIDFor(agentDirPath, sessionKey string) string {
	path := geminiSessionRefPath(agentDirPath, sessionKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !isGeminiSessionID(id) {
		_ = os.Remove(path)
		return ""
	}
	return id
}

func writeGeminiSessionIDFor(agentDirPath, sessionKey, sessionID string, logger *slog.Logger) {
	if !isGeminiSessionID(sessionID) {
		logger.Warn("gemini: refusing to persist malformed session id", "sessionId", sessionID)
		return
	}
	path := geminiSessionRefPath(agentDirPath, sessionKey)
	if err := geminiRefuseSymlinks(openCodeHome(agentDirPath), geminiKojoDir(agentDirPath), filepath.Dir(path)); err != nil {
		logger.Warn("gemini: refusing session ref write", "path", path, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logger.Warn("gemini: mkdir for session ref failed", "path", path, "err", err)
		return
	}
	if err := atomicfile.WriteBytes(path, []byte(sessionID), 0o600); err != nil {
		logger.Warn("gemini: write session ref failed", "path", path, "err", err)
	}
}

// geminiSessionIDPersisted reports whether any stored key resumes
// sessionID, so a OneShot cleanup never deletes a session in use.
func geminiSessionIDPersisted(agentDirPath, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if readGeminiSessionIDFor(agentDirPath, "") == sessionID {
		return true
	}
	entries, err := os.ReadDir(geminiThreadRefDir(agentDirPath))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(geminiThreadRefDir(agentDirPath), e.Name()))
		if rerr == nil && strings.TrimSpace(string(body)) == sessionID {
			return true
		}
	}
	return false
}

// geminiCanResumeSession mirrors grokCanResumeSession for Manager.CanResumeSession.
func geminiCanResumeSession(agentID, sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	dir := agentDir(agentID)
	id := readGeminiSessionIDFor(dir, sessionKey)
	if id == "" {
		return false
	}
	if hasPendingOpenCodeImport(dir, id) {
		return true
	}
	exists, known := openCodeSessionExists(openCodeHome(dir), id)
	return exists || !known
}

// deleteGeminiThreadRefStrict makes the next keyed turn start fresh.
func deleteGeminiThreadRefStrict(agentID, sessionKey string) error {
	err := os.Remove(geminiSessionRefPath(agentDir(agentID), sessionKey))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// clearGeminiSessionCounted removes every OpenCode session (the session
// DB and the legacy JSON storage), every staged import and every kojo
// session ref for the agent. Config and caches are left alone.
func clearGeminiSessionCounted(agentID string) (filesRemoved int, err error) {
	// Serialise with device-switch Read/Stage and pending imports so a
	// reset cannot interleave with a staged swap of the same paths.
	release := lockGeminiSessionTransfer(agentID)
	defer release()
	dir := agentDir(agentID)
	home := openCodeHome(dir)
	// Every directory the removals below go through must be a real
	// directory: the agent can write its own dir, and a planted
	// .opencode-home/data -> ~/.local/share link would otherwise make
	// RemoveAll delete the operator's own OpenCode DB. (The final path
	// elements themselves may be links: RemoveAll removes a link, not
	// its target.)
	if serr := geminiRefuseSymlinks(home, filepath.Join(home, "data"), openCodeDataDir(home),
		geminiKojoDir(dir)); serr != nil {
		return 0, fmt.Errorf("gemini: refusing to clear sessions: %w", serr)
	}
	db := openCodeDBPath(home)
	targets := []string{db, db + "-wal", db + "-shm", db + "-journal",
		filepath.Join(openCodeDataDir(home), "storage"),
		filepath.Join(openCodeDataDir(home), "snapshot"),
		geminiPendingImportDir(dir),
	}
	for _, p := range targets {
		_ = filepath.Walk(p, func(_ string, info os.FileInfo, werr error) error {
			if werr == nil && info != nil && !info.IsDir() {
				filesRemoved++
			}
			return nil
		})
		if rerr := os.RemoveAll(p); rerr != nil && !os.IsNotExist(rerr) && err == nil {
			err = rerr
		}
	}
	if err != nil {
		filesRemoved = 0
	}
	for _, p := range []string{geminiSessionIDFile(dir), geminiThreadRefDir(dir)} {
		if rerr := os.RemoveAll(p); rerr != nil && !os.IsNotExist(rerr) && err == nil {
			err = rerr
		}
	}
	return filesRemoved, err
}

// clearGeminiHome removes the agent's whole OpenCode home (ResetData),
// serialised with device-switch Read/Stage like clearGeminiSessionCounted.
// An explicit reset also removes the pre-OpenCode .gemini-cli-home (the
// gemini-cli era's sessions and state, which ResetData removed back then
// too); a plain migration to OpenCode leaves it in place. RemoveAll
// unlinks a planted symlink without following it.
func clearGeminiHome(agentID string, logger *slog.Logger) {
	release := lockGeminiSessionTransfer(agentID)
	defer release()
	dir := agentDir(agentID)
	if err := os.RemoveAll(openCodeHome(dir)); err != nil && logger != nil {
		logger.Warn("reset: failed to remove opencode home", "agent", agentID, "err", err)
	}
	if err := os.RemoveAll(geminiLegacyCLIHome(dir)); err != nil && logger != nil {
		logger.Warn("reset: failed to remove legacy gemini cli home", "agent", agentID, "err", err)
	}
}

// geminiLegacyCLIHome is the gemini-cli backend's per-agent home.
func geminiLegacyCLIHome(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".gemini-cli-home")
}

// geminiModel returns the concrete model id kojo hands to OpenCode.
func geminiModel(agent *Agent) string {
	return resolveGeminiModel(agent.Model)
}

func resolveGeminiModel(model string) string {
	if model == "" {
		return GeminiDefaultModel
	}
	if concrete := geminiEffortModels[model]; concrete != "" {
		return concrete
	}
	return model
}

// GeminiDefaultMaxOutputTokens is the maxOutputTokens kojo sends when the
// agent has no explicit MaxOutputTokens (0 / unset).
// GeminiMaxOutputTokensLimit is the PATCH upper bound; a value above the
// model's real output limit surfaces as an API 400 on the turn.
const (
	GeminiDefaultMaxOutputTokens = 65536
	GeminiMaxOutputTokensLimit   = 65536
)

// ValidMaxOutputTokens reports whether v is acceptable on PATCH/create
// (0 = reset to default).
func ValidMaxOutputTokens(v int) bool { return v >= 0 && v <= GeminiMaxOutputTokensLimit }

// EffectiveGeminiMaxOutputTokens maps the stored setting to the value sent.
func EffectiveGeminiMaxOutputTokens(v int) int {
	if v <= 0 {
		return GeminiDefaultMaxOutputTokens
	}
	if v > GeminiMaxOutputTokensLimit {
		return GeminiMaxOutputTokensLimit
	}
	return v
}

// geminiVariant maps kojo's effort onto the OpenCode variant kojo defines
// for the model. An empty effort still selects "medium" (the API default)
// explicitly, so the request always carries a thinking config kojo chose.
// Returns "" for models kojo does not describe (no variants defined).
func geminiVariant(model, effort string) string {
	if geminiModelNames[resolveGeminiModel(model)] == "" {
		return ""
	}
	switch effort {
	case "low", "medium", "high":
		return effort
	}
	return "medium"
}

// openCodeGlobalConfig is the static global config written to
// <home>/config/opencode/opencode.json:
//   - autoupdate off, sharing disabled, no git snapshots of the agent dir;
//   - only the google provider is enabled (no other provider's env key or
//     plugin login can be picked up);
//   - every permission allowed except the interactive question tool, so
//     a headless turn never blocks on a prompt;
//   - title generation off (each title would be an extra model request).
func openCodeGlobalConfig() map[string]any {
	return map[string]any{
		"$schema":           "https://opencode.ai/config.json",
		"autoupdate":        false,
		"share":             "disabled",
		"snapshot":          false,
		"lsp":               false,
		"formatter":         false,
		"enabled_providers": []string{"google"},
		"permission":        openCodePermissions(),
		"agent": map[string]any{
			"title": map[string]any{"disable": true},
		},
	}
}

func openCodePermissions() map[string]any {
	return map[string]any{
		"*":                  "allow",
		"external_directory": "allow",
		"doom_loop":          "allow",
		"read":               "allow",
		"question":           "deny",
	}
}

// openCodeEnvPlugin restores the operator's environment for the commands
// the agent runs (shell tool, PTY): the XDG dirs go back to kojo's own
// values, and every variable that exists only to isolate OpenCode (incl.
// the API key) is removed. MCP servers get the same treatment through
// their per-server environment in the turn config.
//
// With openCodeStreamEnv=1 its event hook (called synchronously for every
// bus event of the run's in-process server) also writes, one JSON line
// each on run's own stdout:
//
//	kojo_delta       {sessionID, part{id, type:"text"|"reasoning"}, delta}
//	kojo_tool_start  {sessionID, part{id, type:"tool", tool, callID, state{status:"running", input}}}
//	kojo_child_step  {sessionID, part{id, type:"step-finish", reason, tokens}}
//
// Deltas and tool starts are for the root session only (sessions created
// with a parentID are subagents); child steps carry the subagents' usage,
// which run's own output omits.
const openCodeEnvPlugin = `// Managed by kojo; rewritten on every turn.
const UNSET = "` + openCodeUnset + `"
const XDG = ["XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"]
const DROP = ["OPENCODE_TEST_HOME", "OPENCODE_TEST_MANAGED_CONFIG_DIR", "OPENCODE_CONFIG", "OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX", "OPENCODE_DISABLE_CHANNEL_DB", "GOOGLE_GENERATIVE_AI_API_KEY", "` + customOpenCodeAPIKeyEnv + `"]
const STREAM = process.env["` + openCodeStreamEnv + `"] === "1"
const kinds = new Map()
const children = new Set()
const tools = new Set()
const out = (line) => {
  try {
    process.stdout.write(JSON.stringify(line) + "\n")
  } catch {}
}
export const KojoEnv = async () => ({
  event: async ({ event }) => {
    if (!STREAM || !event || !event.properties) return
    const p = event.properties
    if (event.type === "session.created") {
      if (p.info && p.info.parentID) children.add(p.info.id)
      return
    }
    if (event.type === "message.part.updated") {
      const part = p.part
      if (!part || !part.id) return
      kinds.set(part.id, part.type)
      if (children.has(part.sessionID)) {
        if (part.type === "step-finish" && part.tokens)
          out({ type: "kojo_child_step", sessionID: part.sessionID, part: { id: part.id, type: part.type, reason: part.reason, tokens: part.tokens } })
        return
      }
      if (part.type === "tool" && part.state && part.state.status === "running" && !tools.has(part.id)) {
        tools.add(part.id)
        out({ type: "kojo_tool_start", sessionID: part.sessionID, part: { id: part.id, type: part.type, tool: part.tool, callID: part.callID, state: { status: "running", input: part.state.input } } })
      }
      return
    }
    if (event.type === "message.part.delta") {
      if (p.field !== "text" || typeof p.delta !== "string" || !p.delta || children.has(p.sessionID)) return
      const kind = kinds.get(p.partID)
      if (kind !== "text" && kind !== "reasoning") return
      out({ type: "kojo_delta", sessionID: p.sessionID, part: { id: p.partID, type: kind }, delta: p.delta })
    }
  },
  "shell.env": async (_input, output) => {
    for (const k of XDG) {
      const orig = process.env["KOJO_OC_ORIG_" + k]
      if (orig === undefined) continue
      output.env[k] = orig === UNSET ? undefined : orig
    }
    for (const k of Object.keys(process.env)) {
      if (k.startsWith("KOJO_OC_") || k.startsWith("OPENCODE_DISABLE_") || DROP.includes(k)) output.env[k] = undefined
    }
  },
})
`

// openCodeSeedFiles are written into the global config dir next to
// opencode.json. OpenCode runs `npm install @opencode-ai/plugin` in every
// config dir unless node_modules exists and package-lock.json lists every
// dependency package.json declares (names only); seeding both keeps it
// from ever installing packages from the network.
var openCodeSeedFiles = []struct {
	name string
	body string
}{
	{".gitignore", "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore"},
	{"package.json", `{"dependencies":{"@opencode-ai/plugin":"*"}}` + "\n"},
	{"package-lock.json", `{"lockfileVersion":3,"packages":{"":{"dependencies":{"@opencode-ai/plugin":"*"}}}}` + "\n"},
	{filepath.Join("plugin", "kojo-env.js"), openCodeEnvPlugin},
}

// ensureOpenCodeHome creates the per-agent OpenCode home and (re)writes
// the static global config and seed files. Every directory on the way is
// checked for symlinks: the agent can write its own dir, so a planted
// .opencode-home -> ~ link must not redirect kojo's writes (or OpenCode's
// state) into the operator's real config.
func ensureOpenCodeHome(home string) error {
	cfgDir := openCodeConfigDir(home)
	dirs := []string{
		home,
		filepath.Join(home, "config"), cfgDir, filepath.Join(cfgDir, "plugin"), filepath.Join(cfgDir, "node_modules"),
		filepath.Join(home, "data"), openCodeDataDir(home),
		filepath.Join(home, "state"), filepath.Join(home, "cache"),
		openCodeKojoDir(home),
	}
	if err := geminiRefuseSymlinks(dirs...); err != nil {
		return err
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := geminiRefuseSymlinks(dirs...); err != nil {
		return err
	}
	body, err := marshalOpenCodeConfig(openCodeGlobalConfig())
	if err != nil {
		return err
	}
	if err := writeFileIfChanged(filepath.Join(cfgDir, "opencode.json"), body); err != nil {
		return err
	}
	for _, f := range openCodeSeedFiles {
		if err := writeFileIfChanged(filepath.Join(cfgDir, f.name), []byte(f.body)); err != nil {
			return err
		}
	}
	if err := refuseOpenCodeUnmanaged(home); err != nil {
		return err
	}
	return refuseOpenCodeHomeLinks(home)
}

// openCodeUnmanagedPaths are the places inside the OpenCode home where
// OpenCode 1.19.3 picks up code or config kojo did not write: extra
// global config files (merged over opencode.json; a "plugin" or local
// "mcp" entry there runs with the API key in its env), custom plugins and
// tools ({plugin,plugins,tool,tools}/*.{js,ts} are imported into the
// OpenCode process), the ~/.opencode config dir (os.homedir() is the
// home), the managed config dir (OPENCODE_TEST_MANAGED_CONFIG_DIR, which
// overrides everything) and auth.json (credentials stay in env only).
func openCodeUnmanagedPaths(home string) []string {
	cfgDir := openCodeConfigDir(home)
	return []string{
		filepath.Join(cfgDir, "opencode.jsonc"),
		filepath.Join(cfgDir, "config.json"),
		filepath.Join(cfgDir, "config"),
		filepath.Join(cfgDir, "plugins"),
		filepath.Join(cfgDir, "tool"),
		filepath.Join(cfgDir, "tools"),
		filepath.Join(home, ".opencode"),
		filepath.Join(home, "managed"),
		filepath.Join(openCodeDataDir(home), "auth.json"),
	}
}

// refuseOpenCodeUnmanaged fails the launch when anything OpenCode would
// load as code or config was planted in the home (the agent can write its
// own dir): plugin/ may hold only kojo's plugin, node_modules must stay
// empty (kojo's plugin imports nothing and installs are disabled) and the
// openCodeUnmanagedPaths must not exist. Refusing (rather than deleting)
// leaves the file for the operator to inspect.
func refuseOpenCodeUnmanaged(home string) error {
	for _, p := range openCodeUnmanagedPaths(home) {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("refusing to run opencode: %s is not managed by kojo; remove it", p)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	cfgDir := openCodeConfigDir(home)
	allowed := map[string]map[string]bool{
		"plugin":       {"kojo-env.js": true},
		"node_modules": {},
	}
	for sub, names := range allowed {
		dir := filepath.Join(cfgDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !names[e.Name()] {
				return fmt.Errorf("refusing to run opencode: %s is not managed by kojo; remove it", filepath.Join(dir, e.Name()))
			}
		}
	}
	return nil
}

// openCodeHomeWalkLimit bounds refuseOpenCodeHomeLinks. A home with more
// entries is refused rather than partly checked (a link behind the limit
// would go unseen); a real home holds a few dozen.
const openCodeHomeWalkLimit = 200000

// refuseOpenCodeHomeLinks fails when anything inside the OpenCode home is
// a symlink. OpenCode opens its files (opencode.db and its -wal/-shm
// sidecars, prompt history, logs, locks...) by path and follows links, so
// one planted file link (opencode.db -> ~/.local/share/opencode/...) would
// make the next run read and write the operator's own OpenCode state.
// The directory checks in ensureOpenCodeHome cannot see a link one level
// further down. node_modules is skipped (refuseOpenCodeUnmanaged already
// requires it to be empty).
func refuseOpenCodeHomeLinks(home string) error {
	n := 0
	errLimit := errors.New("walk limit")
	err := filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if n++; n > openCodeHomeWalkLimit {
			return errLimit
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to run opencode: %s is a symlink", p)
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		return nil
	})
	if errors.Is(err, errLimit) {
		return fmt.Errorf("refusing to run opencode: %s holds more than %d entries; cannot verify it has no symlinks", home, openCodeHomeWalkLimit)
	}
	return err
}

// writeFileIfChanged atomically replaces path unless it already holds
// body, so concurrent turns never observe a torn file and an unchanged
// config is not rewritten every turn. A symlink at path is replaced, not
// followed (atomic rename).
func writeFileIfChanged(path string, body []byte) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
		if cur, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(cur, body) {
			return nil
		}
	}
	return atomicfile.WriteBytes(path, body, 0o600)
}

// marshalOpenCodeConfig renders a config map. OpenCode substitutes
// {env:NAME} and {file:path} in the raw config text before parsing it, so
// a system prompt or MCP argument containing those tokens could pull the
// API key or any local file into the request. JSON allows any character
// inside a string to be written as \uXXXX, and both tokens can only occur
// inside strings in marshalled output, so escaping the brace defeats the
// substitution while decoding to the same value.
func marshalOpenCodeConfig(cfg map[string]any) ([]byte, error) {
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	body = bytes.ReplaceAll(body, []byte("{env:"), []byte(`\u007benv:`))
	body = bytes.ReplaceAll(body, []byte("{file:"), []byte(`\u007bfile:`))
	return append(body, '\n'), nil
}

// openCodeTurnConfig is the per-turn config passed via OPENCODE_CONFIG.
type openCodeTurnConfig struct {
	model string // concrete model id
	// custom, when set, replaces the google provider with the
	// custom-opencode preset (see customOpenCodeTurn).
	custom          *customOpenCodeTurn
	variant         string // "" = none
	systemPrompt    string
	maxOutputTokens int // stored agent setting (0 = default)
	mcpServers      map[string]mcpServerEntry
	skillsDir       string
	shell           string // shell-tool shell (see writeOpenCodeShellWrapper); "" = OpenCode's choice
}

// buildOpenCodeTurnConfig assembles the per-turn config:
//   - model / small_model pin every request (incl. compaction and the
//     summary agents) to the agent's model;
//   - agent.build carries the system prompt (it replaces OpenCode's
//     provider prompt; OpenCode still appends its environment block and
//     skill list) and the variant, which the TUI has no flag for;
//   - provider.google.models.<model>.limit.output is the agent's
//     maxOutputTokens. OpenCode sends min(limit.output,
//     OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX || 32000), so the env var is
//     set to the same value (verified by capturing the generateContent
//     request: generationConfig.maxOutputTokens equals the setting);
//   - variants spell out thinkingConfig for low/medium/high with
//     includeThoughts so the reasoning parts reach the stream;
//   - mcp: stdio servers become "local" (their environment restores the
//     operator's XDG dirs and blanks the isolation vars and the key),
//     HTTP servers "remote" with OAuth discovery off;
//   - lsp / formatter are off (as in the global config, repeated because
//     OPENCODE_CONFIG outranks it): OpenCode spawns language servers and
//     formatters with its own environment, which holds the API key;
//   - shell, when set, is the group-logging wrapper of the real shell.
func buildOpenCodeTurnConfig(t openCodeTurnConfig) map[string]any {
	if t.custom != nil {
		return buildCustomOpenCodeTurnConfig(t)
	}
	ref := "google/" + t.model
	maxOut := EffectiveGeminiMaxOutputTokens(t.maxOutputTokens)
	build := map[string]any{"model": ref}
	if t.variant != "" {
		build["variant"] = t.variant
	}
	if t.systemPrompt != "" {
		build["prompt"] = t.systemPrompt
	}
	modelDef := map[string]any{
		"limit": map[string]any{"context": 1048576, "output": maxOut},
	}
	if name := geminiModelNames[t.model]; name != "" {
		modelDef["name"] = name
		modelDef["reasoning"] = true
		modelDef["tool_call"] = true
		modelDef["attachment"] = true
		modelDef["temperature"] = true
		variants := map[string]any{}
		for _, lvl := range []string{"low", "medium", "high"} {
			variants[lvl] = map[string]any{
				"thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": lvl},
			}
		}
		modelDef["variants"] = variants
	}
	cfg := map[string]any{
		"model":       ref,
		"small_model": ref,
		"agent":       map[string]any{"build": build},
		"lsp":         false,
		"formatter":   false,
		"provider": map[string]any{
			"google": map[string]any{
				"models": map[string]any{t.model: modelDef},
			},
		},
	}
	addOpenCodeTurnCommon(cfg, t)
	return cfg
}

// addOpenCodeTurnCommon adds the provider-independent parts of a turn
// config: shell wrapper, skills path and MCP servers.
func addOpenCodeTurnCommon(cfg map[string]any, t openCodeTurnConfig) {
	if t.shell != "" {
		cfg["shell"] = t.shell
	}
	if t.skillsDir != "" {
		cfg["skills"] = map[string]any{"paths": []string{t.skillsDir}}
	}
	if len(t.mcpServers) > 0 {
		mcp := make(map[string]any, len(t.mcpServers))
		for name, srv := range t.mcpServers {
			if srv.isStdio() {
				mcp[name] = map[string]any{
					"type":        "local",
					"command":     append([]string{srv.Command}, srv.Args...),
					"environment": openCodeMCPEnv(srv.Env),
					"enabled":     true,
				}
				continue
			}
			entry := map[string]any{
				"type":    "remote",
				"url":     srv.URL,
				"oauth":   false,
				"enabled": true,
			}
			if len(srv.Headers) > 0 {
				entry["headers"] = srv.Headers
			}
			mcp[name] = entry
		}
		cfg["mcp"] = mcp
	}
}

// openCodeMCPEnv builds a local MCP server's environment. OpenCode spawns
// it with {...process.env, ...environment} and no plugin hook, so the
// isolation values are overridden here: XDG dirs back to kojo's own values
// (empty = unset per the XDG spec); the key and every other variable
// kojo adds only to isolate OpenCode (openCodeIsolationEnv) blank. Values
// the server entry sets itself win: they come from the server's own
// definition, never from kojo's key ({env:} references are escaped by
// marshalOpenCodeConfig).
func openCodeMCPEnv(env map[string]string) map[string]string {
	out := map[string]string{
		"GOOGLE_GENERATIVE_AI_API_KEY": "",
		customOpenCodeAPIKeyEnv:        "",
		"OPENCODE_CONFIG":              "",
		openCodeStreamEnv:              "",
	}
	for _, kv := range openCodeIsolationEnv("", 0) {
		k, _, _ := strings.Cut(kv, "=")
		out[k] = ""
	}
	for _, k := range openCodeXDGVars {
		out[k] = os.Getenv(k)
	}
	for k, v := range env {
		out[k] = v
	}
	return out
}

// openCodeIsolationEnv is appended (after filterEnv) to every OpenCode
// child's environment.
func openCodeIsolationEnv(home string, maxOutputTokens int) []string {
	env := openCodeIsolationBaseEnv(home, maxOutputTokens)
	for _, k := range openCodeXDGVars {
		v, ok := os.LookupEnv(k)
		if !ok {
			v = openCodeUnset
		}
		env = append(env, "KOJO_OC_ORIG_"+k+"="+v)
	}
	return env
}

// openCodeIsolationBaseEnv is openCodeIsolationEnv without the
// KOJO_OC_ORIG_* values (the PTY wrapper captures those from the login
// shell itself).
func openCodeIsolationBaseEnv(home string, maxOutputTokens int) []string {
	return []string{
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		// os.homedir() override: keeps ~/.opencode, ~/.claude and
		// ~/.agents (global config dir, external skills) out of reach.
		"OPENCODE_TEST_HOME=" + home,
		// Points the admin-managed config dir at a path that never exists.
		"OPENCODE_TEST_MANAGED_CONFIG_DIR=" + filepath.Join(home, "managed"),
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
		"OPENCODE_DISABLE_LSP_DOWNLOAD=1",
		"OPENCODE_DISABLE_DEFAULT_PLUGINS=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_CHANNEL_DB=1",
		"OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX=" + strconv.Itoa(EffectiveGeminiMaxOutputTokens(maxOutputTokens)),
	}
}

// geminiCommandEnv builds a chat turn's env: inherited env minus every
// OpenCode/Google knob, plus isolation, the turn config and the key.
func geminiCommandEnv(agentID, dir, apiKey, turnConfig string, maxOutputTokens int) []string {
	return openCodeCommandEnv(agentID, dir, "GOOGLE_GENERATIVE_AI_API_KEY", apiKey, turnConfig, maxOutputTokens)
}

// openCodeCommandEnv is geminiCommandEnv with the key's variable name as a
// parameter (custom-opencode passes customOpenCodeAPIKeyEnv).
func openCodeCommandEnv(agentID, dir, keyEnv, apiKey, turnConfig string, maxOutputTokens int) []string {
	env := filterEnv(geminiStripEnvPrefixes, agentID, dir)
	env = append(env, "PWD="+dir)
	env = append(env, openCodeIsolationEnv(openCodeHome(dir), maxOutputTokens)...)
	if turnConfig != "" {
		env = append(env, "OPENCODE_CONFIG="+turnConfig)
	}
	if apiKey != "" {
		env = append(env, keyEnv+"="+apiKey)
	}
	// Turns on the plugin's live delta lines (chat only: in the PTY they
	// would be written over the TUI).
	env = append(env, openCodeStreamEnv+"=1")
	return env
}

// buildGeminiArgs builds the `opencode run` argv. The prompt goes on stdin
// (OpenCode reads it when no message argument is given), so long prompts
// never hit ARG_MAX and a prompt starting with "-" is never a flag.
//   - --thinking emits the reasoning parts as "reasoning" events;
//   - --auto approves anything still set to "ask";
//   - --variant is not inherited from earlier turns, so it is sent on
//     every turn.
func buildGeminiArgs(model, variant, resumeID string) []string {
	return buildOpenCodeRunArgs("google/"+model, variant, resumeID)
}

// buildOpenCodeRunArgs is buildGeminiArgs for a full "provider/model" ref.
func buildOpenCodeRunArgs(ref, variant, resumeID string) []string {
	args := []string{"run", "--format", "json", "--thinking", "--auto",
		"--title", openCodeChatTitle, "-m", ref}
	if variant != "" {
		args = append(args, "--variant", variant)
	}
	if resumeID != "" {
		args = append(args, "-s", resumeID)
	}
	return args
}

// geminiSkillsDir is where kojo's extension skills for the agent live.
func geminiSkillsDir(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".opencode", "skills")
}

func (b *GeminiBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	ocPath, err := exec.LookPath("opencode")
	if err != nil {
		return nil, fmt.Errorf("opencode not found in PATH")
	}
	var custom *customOpenCodeTurn
	var apiKey string
	if b.isCustom() {
		custom, apiKey, err = b.prepareCustomOpenCode(agent)
		if err != nil {
			return nil, err
		}
	} else {
		apiKey, err = loadGeminiAPIKey(b.creds)
		if err != nil || strings.TrimSpace(apiKey) == "" {
			return nil, errors.New("gemini: no Gemini API key configured (set it in Settings > API keys, or GEMINI_API_KEY)")
		}
		apiKey = strings.TrimSpace(apiKey)
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}
	home := openCodeHome(dir)
	if err := ensureOpenCodeHome(home); err != nil {
		return nil, fmt.Errorf("prepare opencode home: %w", err)
	}
	importPendingOpenCodeSessions(ctx, agent.ID, b.logger)

	// Resume strategy matches the grok backend: !OneShot resumes the
	// ref stored for opts.SessionKey ("" = the agent's own session);
	// OneShot always starts fresh and never touches a stored ref.
	persistKey := opts.SessionKey
	var resumeID string
	if !opts.OneShot {
		resumeID = readGeminiSessionIDFor(dir, persistKey)
		if resumeID != "" && hasPendingOpenCodeImport(dir, resumeID) {
			// importPendingOpenCodeSessions postponed this session's
			// device-switch import (it keeps retrying, then parks the
			// export as *.failed). Until then neither resume the stale
			// local copy nor drop the ref: fail the turn so it can be
			// retried once the import lands.
			return nil, fmt.Errorf("gemini: session %s from the device switch is not imported yet; try again shortly", resumeID)
		}
		if resumeID != "" {
			if exists, known := openCodeSessionExists(home, resumeID); known && !exists {
				_ = os.Remove(geminiSessionRefPath(dir, persistKey))
				b.logger.Info("gemini: dropped session ref with no session in the opencode db",
					"agent", agent.ID, "staleId", resumeID)
				resumeID = ""
			} else if bad, got := openCodeSessionDirMismatch(home, resumeID, dir); bad {
				_ = os.Remove(geminiSessionRefPath(dir, persistKey))
				b.logger.Warn("gemini: dropped session ref whose opencode session belongs to another directory",
					"agent", agent.ID, "staleId", resumeID, "sessionDir", got, "agentDir", dir)
				resumeID = ""
			}
		}
	}
	// Held until the turn's process exits so a concurrent turn on the same
	// session never prunes under a live run.
	releaseLive := func() {}
	if resumeID != "" {
		releaseLive = acquireOpenCodeLiveSession(home, resumeID, func() {
			pruneOpenCodeSessionLogged(ctx, home, agent.ID, resumeID, b.logger)
		})
		// The wait is bounded by openCodePruneTimeout; a turn aborted
		// meanwhile must not start OpenCode.
		if err := ctx.Err(); err != nil {
			releaseLive()
			return nil, err
		}
	}
	handedOff := false
	defer func() {
		if !handedOff {
			releaseLive()
		}
	}()
	buildMessage := func(resuming bool) string {
		msg := injectSessionContexts(userMessage, opts, resuming)
		if !opts.OneShot && !resuming && opts.FreshSessionContext == "" && opts.RecentMessagesContext != "" {
			msg = injectRecentMessagesContext(msg, opts.RecentMessagesContext)
		}
		return msg
	}

	model, variant, ref := "", "", ""
	keyEnv, maxOut := "GOOGLE_GENERATIVE_AI_API_KEY", agent.MaxOutputTokens
	if custom != nil {
		model, ref = custom.model, custom.ref()
		keyEnv = customOpenCodeAPIKeyEnv
		maxOut = effectiveOpenCodeMaxOutputTokens(ToolCustomOpenCode, agent.MaxOutputTokens)
	} else {
		model = geminiModel(agent)
		variant = geminiVariant(model, agent.Effort)
		ref = "google/" + model
	}
	// The turn's shell commands log their process groups so an aborted
	// turn can reap the background jobs they leave (see opencode_shell.go).
	shellDir, shell := "", ""
	if runtime.GOOS != "windows" {
		sweepOpenCodeShellDirs(home, time.Now().Add(-openCodeShellDirMaxAge))
		shellDir = newOpenCodeChatShellDir(home)
		sh, werr := writeOpenCodeShellWrapper(home, shellDir)
		if werr != nil {
			// Only a planted symlink or an I/O error gets here; running
			// without the wrapper would leave an aborted turn's
			// background jobs unreaped.
			_ = os.RemoveAll(shellDir)
			return nil, fmt.Errorf("gemini: prepare shell wrapper: %w", werr)
		}
		shell = sh
	}
	removeShellDir := func() {
		if shellDir != "" {
			_ = os.RemoveAll(shellDir)
		}
	}
	cfgBody, err := renderOpenCodeTurnConfig(openCodeTurnConfig{
		model:           model,
		custom:          custom,
		variant:         variant,
		systemPrompt:    systemPrompt,
		maxOutputTokens: maxOut,
		mcpServers:      opts.MCPServers,
		skillsDir:       geminiSkillsDir(dir),
		shell:           shell,
	})
	if err != nil {
		removeShellDir()
		return nil, fmt.Errorf("build opencode config: %w", err)
	}
	// Each turn gets its own config file: the system prompt varies per
	// turn (keyed surfaces append their own context) and concurrent turns
	// for the same agent must never overwrite each other's.
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	turnConfig := filepath.Join(openCodeKojoDir(home), "turn-"+hex.EncodeToString(rnd[:])+".json")
	if err := os.WriteFile(turnConfig, cfgBody, 0o600); err != nil {
		removeShellDir()
		return nil, fmt.Errorf("write opencode config: %w", err)
	}
	removeTurnConfig := func() {
		_ = os.Remove(turnConfig)
		removeShellDir()
	}

	env := appendKojoTurnEnv(openCodeCommandEnv(agent.ID, dir, keyEnv, apiKey, turnConfig, maxOut), opts)
	start := func(resume string) (*openCodeProc, error) {
		return startOpenCode(ctx, ocPath, buildOpenCodeRunArgs(ref, variant, resume), env, dir, buildMessage(resume != ""))
	}
	proc, err := start(resumeID)
	if err != nil {
		removeTurnConfig()
		return nil, fmt.Errorf("start opencode: %w", err)
	}

	ch := make(chan ChatEvent, 64)
	handedOff = true
	go func() {
		defer close(ch)
		defer releaseLive()
		defer removeTurnConfig()

		send := func(e ChatEvent) bool { return ctxSend(ctx, ch, e) }
		var result *geminiStreamResult
		var waitErr error
		for {
			result = parseGeminiStream(proc.stdout, b.logger, send)
			if result.cancelled {
				break
			}
			waitErr = proc.wait()
			// A ref whose session is gone (e.g. the DB was unreadable
			// for the pre-check): OpenCode prints "Session not found"
			// before emitting anything. Drop the ref and run the turn
			// once more as a fresh session.
			if resumeID != "" && !result.anyEvent && isGeminiStaleSessionError(proc.stderr.String()) {
				_ = os.Remove(geminiSessionRefPath(dir, persistKey))
				b.logger.Info("gemini: dropped stale session ref after resume failure",
					"agent", agent.ID, "staleId", resumeID)
				resumeID = ""
				next, serr := start("")
				if serr != nil {
					send(ChatEvent{Type: "error", ErrorMessage: "start opencode: " + serr.Error()})
					return
				}
				proc = next
				continue
			}
			break
		}

		cleanupOneShot := func() {
			if opts.OneShot && result.sessionID != "" && !geminiSessionIDPersisted(dir, result.sessionID) {
				deleteOpenCodeSession(ocPath, agent.ID, dir, result.sessionID, b.logger)
			}
		}

		if result.cancelled {
			_ = proc.wait()
		}
		if result.cancelled || ctx.Err() != nil {
			// Aborted (the stream may also just have ended when the tree
			// kill closed it): the tree kill misses background jobs whose
			// shell already exited; they are still in the shell's logged
			// group.
			proc.awaitKill()
			reapOpenCodeShellDir(home, shellDir, b.logger)
		}

		if result.cancelled {
			emitCancelDone(ctx, ch, result.text, result.thinking, result.toolUses, result.usage)
			cleanupOneShot()
			return
		}

		if result.usage != nil && custom == nil {
			result.usage.CostUSD = geminiUsageCostUSD(model, result.usage, geminiNow())
		}
		processError := classifyGeminiProcessError(waitErr, proc.stderr.String(), result)
		if processError != "" {
			b.logger.Warn("gemini turn failed",
				"agent", agent.ID, "err", waitErr, "stderr", proc.stderr.String(), "streamError", result.streamError)
		}

		if processError != "" && result.text == "" {
			cleanupOneShot()
			send(ChatEvent{Type: "error", ErrorMessage: processError})
			return
		}

		if !opts.OneShot && resumeID == "" && result.sessionID != "" {
			writeGeminiSessionIDFor(dir, persistKey, result.sessionID, b.logger)
		}
		cleanupOneShot()

		msg := assembleAssistantMessage(result.text, result.thinking, result.toolUses, result.usage)
		send(ChatEvent{Type: "done", Message: msg, Usage: result.usage, ErrorMessage: processError})
	}()

	return ch, nil
}

// openCodeProc is one running `opencode run`.
type openCodeProc struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr *bytes.Buffer
	done   chan struct{}
	killed chan struct{} // closed when the ctx watcher returns
	err    error
}

// startOpenCode starts OpenCode with ctx-driven tree kill. exec.CommandContext
// is not used: it would signal only the opencode process, and OpenCode
// starts every shell-tool command detached in its own process group with
// no signal handler to reap them, so a cancelled turn would orphan e.g. a
// running `sleep 600`. The watcher snapshots the whole descendant tree
// and terminates it instead (SIGTERM, then SIGKILL after 3s; taskkill /T
// on Windows).
func startOpenCode(ctx context.Context, path string, args, env []string, dir, stdin string) (*openCodeProc, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.WaitDelay = 10 * time.Second
	setProcGroup(cmd)
	p := &openCodeProc{cmd: cmd, stderr: &bytes.Buffer{}, done: make(chan struct{}), killed: make(chan struct{})}
	cmd.Stderr = &limitedWriter{w: p.stderr, limit: 4096}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	p.stdout = stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	// The start time recorded now lets the watcher tell opencode from an
	// unrelated process that got its pid after wait reaped it (wait's
	// cmd.Wait reaps before p.done closes, and a cancel can land in
	// between).
	ident := proctree.Identify(pid)
	go func() {
		defer close(p.killed)
		select {
		case <-ctx.Done():
			select {
			case <-p.done: // exited and reaped already: nothing to kill
				return
			default:
			}
			proctree.KillTreeIdent(pid, ident, 3*time.Second)
		case <-p.done:
		}
	}()
	return p, nil
}

// awaitKill waits until the ctx watcher is done (its tree kill finished,
// or it saw the process exit first). Call after wait.
func (p *openCodeProc) awaitKill() { <-p.killed }

// wait reaps the process. Safe to call more than once.
func (p *openCodeProc) wait() error {
	select {
	case <-p.done:
		return p.err
	default:
	}
	p.err = p.cmd.Wait()
	close(p.done)
	return p.err
}
