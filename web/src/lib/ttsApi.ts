import { get, post } from "./httpClient";
import { appendTokenQuery } from "./auth";

export interface VoiceInfo {
  name: string;
  trait: string; // "Bright", "Upbeat", "Firm", ...
  gender?: "F" | "M" | "";
}

export interface TTSCapability {
  ffmpeg: boolean;
  formats: string[]; // ["opus", "mp3", "wav"] — server-supported
  voices: string[];  // flat list of voice ids (legacy)
  voiceCatalog: VoiceInfo[]; // voice id + descriptive trait
  models: string[];  // accepted model ids
  defaults: {
    model: string;
    voice: string;
    stylePrompt: string;
  };
}

export interface TTSSynthesizeResponse {
  hash: string;
  format: string;
  url: string;     // server path; needs token query for direct <audio> use
  bytes: number;
  cached: boolean;
}

export const ttsApi = {
  capability: () => get<TTSCapability>("/api/v1/tts/capability"),

  synthesize: (
    agentId: string,
    text: string,
    format: "opus" | "mp3" | "wav",
  ) =>
    post<TTSSynthesizeResponse>(
      `/api/v1/agents/${agentId}/tts/synthesize`,
      { text, format },
    ),

  // preview synthesizes a fixed sample line so the user can audition
  // a voice before saving it to the agent. Cached on the server like
  // any other synthesize request, so re-listening to the same voice
  // is essentially free.
  preview: (
    voice: string,
    opts: { model?: string; stylePrompt?: string; format?: "opus" | "mp3" | "wav" } = {},
  ) =>
    post<TTSSynthesizeResponse>("/api/v1/tts/preview", {
      voice,
      model: opts.model,
      stylePrompt: opts.stylePrompt,
      format: opts.format ?? "opus",
    }),

  // audioUrl converts a server-relative URL into one that includes the
  // owner-token query parameter so it can go straight into <audio src>.
  audioUrl: (path: string) => appendTokenQuery(path),
};

// pickBestFormat picks the smallest format the *current browser* can
// actually play. The server's supported list narrows what we may ask for
// (e.g. ffmpeg-less servers can only emit wav).
export function pickBestFormat(
  serverFormats: string[],
): "opus" | "mp3" | "wav" {
  const audio = typeof Audio !== "undefined" ? new Audio() : null;
  const can = (mime: string) =>
    !!audio && audio.canPlayType(mime) !== "";

  if (serverFormats.includes("opus") && can("audio/ogg; codecs=opus")) {
    return "opus";
  }
  if (serverFormats.includes("mp3") && can("audio/mpeg")) {
    return "mp3";
  }
  return "wav";
}
