package tts

// VoiceInfo pairs a voice id with the descriptive trait Google publishes
// in the Gemini TTS docs and the gender label Google publishes for the
// matching Cloud Text-to-Speech Chirp3-HD voice. Both are "official";
// Gender is "F" / "M" / "" (unknown).
//
// Trait source: https://ai.google.dev/gemini-api/docs/speech-generation
// Gender source: https://docs.cloud.google.com/text-to-speech/docs/list-voices-and-types
//   (the same voice names appear under Chirp3-HD with ssmlGender annotated)
type VoiceInfo struct {
	Name   string `json:"name"`
	Trait  string `json:"trait"`
	Gender string `json:"gender,omitempty"` // "F" | "M" | ""
}

// VoiceCatalog is the canonical, ordered list of voices supported by
// Gemini TTS as of the speech-generation docs in 2026-05.
var VoiceCatalog = []VoiceInfo{
	{"Zephyr", "Bright", "F"},
	{"Puck", "Upbeat", "M"},
	{"Charon", "Informative", "M"},
	{"Kore", "Firm", "F"},
	{"Fenrir", "Excitable", "M"},
	{"Leda", "Youthful", "F"},
	{"Orus", "Firm", "M"},
	{"Aoede", "Breezy", "F"},
	{"Callirrhoe", "Easy-going", "F"},
	{"Autonoe", "Bright", "F"},
	{"Enceladus", "Breathy", "M"},
	{"Iapetus", "Clear", "M"},
	{"Umbriel", "Easy-going", "M"},
	{"Algieba", "Smooth", "M"},
	{"Despina", "Smooth", "F"},
	{"Erinome", "Clear", "F"},
	{"Algenib", "Gravelly", "M"},
	{"Rasalgethi", "Informative", "M"},
	{"Laomedeia", "Upbeat", "F"},
	{"Achernar", "Soft", "F"},
	{"Alnilam", "Firm", "M"},
	{"Schedar", "Even", "M"},
	{"Gacrux", "Mature", "F"},
	{"Pulcherrima", "Forward", "F"},
	{"Achird", "Friendly", "M"},
	{"Zubenelgenubi", "Casual", "M"},
	{"Vindemiatrix", "Gentle", "F"},
	{"Sadachbia", "Lively", "M"},
	{"Sadaltager", "Knowledgeable", "M"},
	{"Sulafat", "Warm", "F"},
}

// Voices is a flat list of voice ids preserved for backwards compatibility
// with the existing IsValidVoice/Service signatures.
var Voices = func() []string {
	out := make([]string, len(VoiceCatalog))
	for i, v := range VoiceCatalog {
		out[i] = v.Name
	}
	return out
}()

// IsValidVoice reports whether the given name is in the canonical voice list.
// Empty string is rejected.
func IsValidVoice(name string) bool {
	if name == "" {
		return false
	}
	for _, v := range VoiceCatalog {
		if v.Name == name {
			return true
		}
	}
	return false
}

// Models is the set of TTS models we accept from clients.
var Models = []string{
	"gemini-3.1-flash-tts-preview",
	"gemini-2.5-flash-preview-tts",
	"gemini-2.5-pro-preview-tts",
}

// IsValidModel reports whether the given model id is in the accepted list.
func IsValidModel(name string) bool {
	for _, v := range Models {
		if v == name {
			return true
		}
	}
	return false
}
