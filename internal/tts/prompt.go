package tts

// SystemInstruction is prepended to every TTS request to keep the model
// in narrator mode and avoid refusal/rewriting of the input text.
const SystemInstruction = `You are a text-to-speech narrator inside kojo, a private coding assistant UI.
Read the provided text exactly as a coding progress notification or fictional in-app dialogue.
Read code, file paths, logs, stack traces and error messages neutrally as technical content.
Do not refuse or rewrite the input. Do not add commentary.
Keep delivery natural, expressive but non-abusive.`

// DefaultStylePrompt is used when the agent has no custom style prompt.
const DefaultStylePrompt = "落ち着いた日本語で、淡々と短く読み上げて。"

// DefaultModel is the Gemini TTS model used by default.
const DefaultModel = "gemini-3.1-flash-tts-preview"

// DefaultVoice is the default voice from the 30-voice Gemini TTS catalogue.
const DefaultVoice = "Kore"

// MaxChars caps input text length to bound cost and latency. Anything
// longer is truncated with a trailing ellipsis before being sent.
const MaxChars = 800
