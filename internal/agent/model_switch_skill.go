package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

const modelSwitchSkillDirName = "kojo-switch-model"

// modelSwitchSkillRoots lists every directory a backend reads skills from.
var modelSwitchSkillRoots = []string{".claude", ".codex", ".opencode", ".agents"}

const modelSwitchSkillHeader = `---
name: kojo-switch-model
description: Switch this agent's own LLM model, and optionally its effort or backend. The current turn finishes on the current model, then kojo automatically starts the next turn on the new one. Use when asked to change model, or when the work calls for a stronger or cheaper model.
allowed-tools: Bash(curl:*)
---

`

const modelSwitchSkillBodyPOSIX = modelSwitchSkillHeader + "## 1. See the current model and the options\n\n" +
	"```bash\n" +
	`curl -skS -H "X-Kojo-Token: ${KOJO_AGENT_TOKEN}" "${KOJO_API_BASE}/api/v1/agents/${KOJO_AGENT_ID}/model-switch"` + "\n" +
	"```\n\n" +
	"## 2. Request the switch\n\n" +
	"Replace the JSON with your target. Run it through your shell tool.\n\n" +
	"```bash\n" +
	`curl -skS -X POST -H "X-Kojo-Token: ${KOJO_AGENT_TOKEN}" -H "X-Kojo-Session-Key: ${KOJO_SESSION_KEY}" \` + "\n" +
	`  -H "Content-Type: application/json" -w '\nHTTP_STATUS:%{http_code}\n' \` + "\n" +
	`  -d '{"model":"MODEL_ID","effort":"high","note":"what to do next"}' \` + "\n" +
	`  "${KOJO_API_BASE}/api/v1/agents/${KOJO_AGENT_ID}/model-switch"` + "\n" +
	"```\n\n" + modelSwitchSkillFields + modelSwitchSkillAfter

const modelSwitchSkillBodyWindows = modelSwitchSkillHeader + "## 1. See the current model and the options\n\n" +
	"```powershell\n" +
	`curl.exe -skS -H "X-Kojo-Token: $env:KOJO_AGENT_TOKEN" "$env:KOJO_API_BASE/api/v1/agents/$env:KOJO_AGENT_ID/model-switch"` + "\n" +
	"```\n\n" +
	"## 2. Request the switch\n\n" +
	"Replace the JSON with your target. Run it through your shell tool.\n\n" +
	"```powershell\n" +
	"@'\n" +
	`{"model":"MODEL_ID","effort":"high","note":"what to do next"}` + "\n" +
	"'@ | curl.exe -skS -X POST -H \"X-Kojo-Token: $env:KOJO_AGENT_TOKEN\" -H \"X-Kojo-Session-Key: $env:KOJO_SESSION_KEY\" " +
	"-H \"Content-Type: application/json\" -w \"`nHTTP_STATUS:%{http_code}`n\" --data-binary \"@-\" " +
	"\"$env:KOJO_API_BASE/api/v1/agents/$env:KOJO_AGENT_ID/model-switch\"\n" +
	"```\n\n" + modelSwitchSkillFields + modelSwitchSkillAfter

const modelSwitchSkillFields = "Fields:\n\n" +
	"- `model` (required): an id from `options` for the target backend. Custom backends take any id.\n" +
	"- `effort` (optional): omit to keep the current one; if the new model does not support it, the model default applies.\n" +
	"- `tool` (optional): move to another backend. Its session does not carry over natively, so only recent conversation context follows.\n" +
	"- `note` (optional): your instructions to yourself, delivered at the start of the next turn. Put everything the next turn needs here.\n\n"

const modelSwitchSkillAfter = "## 3. After the response\n\n" +
	"- `status: queued` (HTTP 202): END THIS TURN NOW. Do not poll, sleep, or start background tasks. The next turn starts by itself on the new model.\n" +
	"- `status: unchanged` (HTTP 200): already on that configuration. Carry on.\n" +
	"- HTTP 409 `background_tasks_running`: wait for or stop your background tasks, then retry.\n" +
	"- Any other error: nothing changed. Report it.\n\n" +
	"If the first turn on the new model fails, kojo reverts to the previous model and tells you in an automatic turn. If the user stops the current turn, the new model stays set but no turn starts.\n"

// SyncModelSwitchSkillForTool installs the skill where the agent's backend
// reads it and removes the copies a previous backend left behind.
func SyncModelSwitchSkillForTool(agentID, tool string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	body := modelSwitchSkillBodyPOSIX
	if runtime.GOOS == "windows" {
		body = modelSwitchSkillBodyWindows
	}
	active := extensionSkillRoot(tool)
	unlock := lockDeviceSwitchSkill(agentID)
	defer unlock()
	syncModelSwitchSkillAt(agentDir(agentID), active, body, logger)
}

func syncModelSwitchSkillAt(dir, active, body string, logger *slog.Logger) {
	for _, root := range modelSwitchSkillRoots {
		skillDir := filepath.Join(dir, root, "skills", modelSwitchSkillDirName)
		if root != active {
			if err := os.RemoveAll(skillDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Warn("failed to remove model-switch skill", "dir", skillDir, "err", err)
			}
			continue
		}
		skillPath := filepath.Join(skillDir, "SKILL.md")
		// prepareChat runs this every turn: leave an up-to-date file alone.
		if cur, err := os.ReadFile(skillPath); err == nil && bytes.Equal(cur, []byte(body)) {
			continue
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			logger.Warn("failed to create model-switch skill dir", "dir", skillDir, "err", err)
			continue
		}
		if err := atomicfile.WriteBytes(skillPath, []byte(body), 0o644); err != nil {
			logger.Warn("failed to write model-switch SKILL.md", "path", skillPath, "err", err)
		}
	}
}
