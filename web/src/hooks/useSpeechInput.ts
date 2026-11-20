import { useCallback, useEffect, useRef, useState } from "react";
import { agentApi, type STTTokenResponse } from "../lib/agentApi";
import { errMsg } from "../lib/utils";

// Streaming toggle-to-talk speech-to-text against the globally selected STT
// provider (xAI or Gemini Live; Settings → Chat → Voice input).
//
// Flow: acquire the mic → mint an ephemeral token from our backend (which
// also tells us the provider) → open the provider WebSocket directly → wait
// for its ready event → downsample mic audio to 16 kHz mono PCM16 in an
// AudioWorklet and stream it → surface interim partials live and commit
// finalized utterances via onFinal.
//
// xAI (docs.x.ai, Streaming Speech-to-Text): token in the
// `xai-client-secret.<token>` subprotocol (browsers can't set Authorization
// on a WebSocket).
//   client → binary PCM frames; {"type":"audio.done"} to end.
//   server → transcript.created | transcript.partial | transcript.done | error
//   transcript.partial: is_final=false → interim; is_final=true,
//     speech_final=false → chunk final; both true → utterance final.
//
// Gemini (ai.google.dev, Live API transcription, gemini-3.5-transcribe-live):
// token in the `access_token` query of BidiGenerateContentConstrained.
//   client → {setup} first; {realtimeInput:{audio:{data:<b64>,mimeType}}};
//     {realtimeInput:{audioStreamEnd:true}} to flush on stop.
//   server → setupComplete | serverContent.interimInputTranscription (the
//     current utterance so far, replaces) | serverContent.inputTranscription
//     (utterance final) | voiceActivity ACTIVITY_START/ACTIVITY_END.
//   Errors arrive as a close frame with a reason.

export type SpeechInputState = "idle" | "connecting" | "listening" | "error";

const TARGET_RATE = 16000;
const CLIENT_SECRET_PREFIX = "xai-client-secret.";
// Gemini wants JSON+base64 frames, so batch the worklet's tiny render-quantum
// chunks into ~100 ms (1600 samples = 3200 bytes) messages.
const GEMINI_CHUNK_BYTES = 3200;
const GEMINI_MIME = `audio/pcm;rate=${TARGET_RATE}`;
// How long stop() waits for the provider's final transcript before closing.
const FLUSH_TIMEOUT_MS = 3000;
// Gemini: after audioStreamEnd with no utterance in flight, how long to wait
// for a late voice-activity / transcript event before closing.
const GEMINI_IDLE_GRACE_MS = 1000;

/** Map a browser locale (e.g. "ja-JP", "en-US") to an xAI language code. */
function localeToLanguage(locale: string): string {
  const primary = (locale || "en").toLowerCase().split("-")[0];
  return primary || "en";
}

// Default regions for bare browser locales, so Gemini gets a full BCP-47
// code. Unknown bare languages fall back to auto-detection ("").
const DEFAULT_REGION: Record<string, string> = {
  ja: "ja-JP",
  en: "en-US",
  zh: "zh-CN",
  ko: "ko-KR",
  fr: "fr-FR",
  de: "de-DE",
  es: "es-ES",
  it: "it-IT",
  pt: "pt-BR",
  ru: "ru-RU",
};

/** Map a browser locale to a Gemini BCP-47 language code ("" = auto). */
export function localeToBCP47(locale: string): string {
  const tag = (locale || "").trim();
  if (!/^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$/.test(tag)) return "";
  const parts = tag.split("-");
  if (parts.length > 1) return `${parts[0].toLowerCase()}-${parts.slice(1).join("-")}`;
  return DEFAULT_REGION[parts[0].toLowerCase()] ?? "";
}

function bytesToBase64(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(bin);
}

// Provider-specific halves of a live session.
interface Transport {
  /** Forward one PCM16 chunk (only called once ready). */
  sendAudio: (buf: ArrayBuffer) => void;
  /** Stop requested: flush pending audio and ask for the final transcript. */
  finish: () => void;
}

interface Options {
  /** Called for each finalized utterance; the composer appends this. */
  onFinal: (text: string) => void;
}

export interface SpeechInput {
  state: SpeechInputState;
  interimText: string;
  error: string;
  /** True only in secure contexts with a usable mic capture API. */
  supported: boolean;
  start: () => Promise<void>;
  stop: () => void;
}

// A minimal shape for the xAI transcript events we consume.
interface TranscriptEvent {
  type: string;
  text?: string;
  is_final?: boolean;
  speech_final?: boolean;
  message?: string;
}

// A minimal shape for the Gemini Live server messages we consume.
interface GeminiServerMessage {
  setupComplete?: object;
  serverContent?: {
    interimInputTranscription?: { text?: string };
    inputTranscription?: { text?: string };
  };
  voiceActivity?: { type?: string };
  error?: { message?: string };
}

export function useSpeechInput({ onFinal }: Options): SpeechInput {
  const [state, setState] = useState<SpeechInputState>("idle");
  const [interimText, setInterimText] = useState("");
  const [error, setError] = useState("");

  // Latest onFinal without retriggering start()'s identity.
  const onFinalRef = useRef(onFinal);
  onFinalRef.current = onFinal;

  const wsRef = useRef<WebSocket | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const ctxRef = useRef<AudioContext | null>(null);
  const nodeRef = useRef<AudioWorkletNode | ScriptProcessorNode | null>(null);
  const sourceRef = useRef<MediaStreamAudioSourceNode | null>(null);
  const readyRef = useRef(false);
  const transportRef = useRef<Transport | null>(null);
  // Set once stop() has asked the provider for its final transcript; the
  // next final (or the socket close) completes teardown without an error.
  const finishingRef = useRef(false);
  // Resolves when a finishing session tears down, so a start() issued while
  // the previous session is still flushing waits instead of killing it.
  const finishWaitRef = useRef<{ promise: Promise<void>; resolve: () => void } | null>(null);
  // Bumped by every start() and stop(); a start() waiting on a flushing
  // session aborts if it changed (the user cancelled or restarted again).
  const startSeqRef = useRef(0);
  // Whether an interim (not-yet-final) partial has arrived since the last
  // committed utterance, so transcript.done's flush only appends genuinely
  // pending text instead of re-appending already-committed utterances.
  const pendingInterimRef = useRef(false);
  // Text of the last committed utterance, so transcript.done can't re-append
  // an utterance already emitted via speech_final.
  const lastCommittedRef = useRef("");
  // Session generation: bumped on every start() and teardown(). An async
  // continuation whose captured generation no longer matches has been
  // superseded (stopped/unmounted) and must abort instead of reviving audio.
  const genRef = useRef(0);

  const supported =
    typeof window !== "undefined" &&
    window.isSecureContext &&
    typeof navigator !== "undefined" &&
    !!navigator.mediaDevices?.getUserMedia &&
    typeof AudioContext !== "undefined";

  // Tear down all audio + socket resources. Safe to call repeatedly. Bumps
  // the generation so any in-flight start() continuation aborts.
  const teardown = useCallback((closeWS: boolean) => {
    genRef.current++;
    readyRef.current = false;
    finishingRef.current = false;
    finishWaitRef.current?.resolve();
    finishWaitRef.current = null;
    transportRef.current = null;
    pendingInterimRef.current = false;
    if (nodeRef.current && "port" in nodeRef.current) {
      (nodeRef.current as AudioWorkletNode).port.onmessage = null;
    }
    try {
      nodeRef.current?.disconnect();
    } catch {
      /* already gone */
    }
    nodeRef.current = null;
    try {
      sourceRef.current?.disconnect();
    } catch {
      /* already gone */
    }
    sourceRef.current = null;
    streamRef.current?.getTracks().forEach((t) => t.stop());
    streamRef.current = null;
    if (ctxRef.current && ctxRef.current.state !== "closed") {
      void ctxRef.current.close();
    }
    ctxRef.current = null;
    if (closeWS && wsRef.current) {
      const ws = wsRef.current;
      wsRef.current = null;
      try {
        ws.close();
      } catch {
        /* already closing */
      }
    }
  }, []);

  const fail = useCallback(
    (msg: string) => {
      teardown(true);
      setError(msg);
      setState("error");
      setInterimText("");
    },
    [teardown],
  );

  const start = useCallback(async () => {
    if (!supported) {
      fail("Voice input is not available in this browser.");
      return;
    }
    const seq = ++startSeqRef.current;
    const flushing = finishWaitRef.current;
    if (flushing) {
      // The previous session is still delivering its final transcript; let
      // it land before replacing the socket.
      setState("connecting");
      await flushing.promise;
      if (startSeqRef.current !== seq) return;
    }
    // Begin a fresh session; capture its generation to detect supersession.
    teardown(true);
    const gen = genRef.current;
    setError("");
    setInterimText("");
    pendingInterimRef.current = false;
    lastCommittedRef.current = "";
    setState("connecting");

    // 1. Acquire the mic first — a permission prompt can take seconds, and we
    //    don't want to mint a short-lived token (or open a socket) until we
    //    know capture will actually happen.
    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true, channelCount: 1 },
      });
    } catch (err) {
      if (genRef.current !== gen) return;
      fail(errMsg(err));
      return;
    }
    if (genRef.current !== gen) {
      // Superseded while awaiting permission — drop the just-granted stream.
      stream.getTracks().forEach((t) => t.stop());
      return;
    }
    streamRef.current = stream;

    // 2. Mint an ephemeral token from our backend. The response names the
    //    provider the server has in effect.
    let minted: STTTokenResponse;
    try {
      minted = await agentApi.stt.token(localeToBCP47(navigator.language));
    } catch (err) {
      if (genRef.current !== gen) return;
      fail(errMsg(err));
      return;
    }
    if (genRef.current !== gen) return;

    // 3. Open the provider's STT WebSocket.
    let ws: WebSocket;
    try {
      if (minted.provider === "gemini") {
        ws = new WebSocket(`${minted.wsBaseUrl}?access_token=${encodeURIComponent(minted.token)}`);
      } else {
        const lang = localeToLanguage(navigator.language);
        const url =
          `${minted.wsBaseUrl}?sample_rate=${TARGET_RATE}&encoding=pcm&interim_results=true` +
          `&language=${encodeURIComponent(lang)}`;
        ws = new WebSocket(url, [CLIENT_SECRET_PREFIX + minted.token]);
      }
    } catch (err) {
      fail(errMsg(err));
      return;
    }
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;

    ws.onerror = () => {
      setError((e) => e || "Voice connection error.");
    };
    ws.onclose = (ev: CloseEvent) => {
      // Intended closes route through teardown(), which nulls wsRef and bumps
      // the generation, so reaching here with this ws still current means the
      // server closed the socket (auth failure, network, token expiry, or the
      // provider finishing after stop()). Tear down the mic/AudioContext and
      // surface an error unless we were already finishing.
      if (wsRef.current !== ws || genRef.current !== gen) return;
      const finishing = finishingRef.current;
      wsRef.current = null;
      teardown(false);
      setInterimText("");
      if (finishing) {
        setState("idle");
        return;
      }
      const reason = ev.reason ? `: ${ev.reason}` : "";
      setError((e) => e || `Voice connection closed unexpectedly${reason}`);
      setState("error");
    };

    const commit = (text: string) => {
      if (text) {
        lastCommittedRef.current = text;
        onFinalRef.current(text);
      }
      pendingInterimRef.current = false;
      setInterimText("");
    };

    if (minted.provider === "gemini") {
      // An utterance the server VAD started whose final transcript hasn't
      // arrived yet; stop() waits for it.
      let speaking = false;
      let pending = new Uint8Array(0);
      const flushAudio = () => {
        if (pending.length === 0 || ws.readyState !== WebSocket.OPEN) return;
        ws.send(
          JSON.stringify({
            realtimeInput: { audio: { data: bytesToBase64(pending), mimeType: GEMINI_MIME } },
          }),
        );
        pending = new Uint8Array(0);
      };
      const finishIfDone = () => {
        if (finishingRef.current && !speaking && !pendingInterimRef.current && wsRef.current === ws && genRef.current === gen) {
          teardown(true);
          setState("idle");
        }
      };
      transportRef.current = {
        sendAudio: (buf) => {
          const chunk = new Uint8Array(buf);
          const next = new Uint8Array(pending.length + chunk.length);
          next.set(pending);
          next.set(chunk, pending.length);
          pending = next;
          if (pending.length >= GEMINI_CHUNK_BYTES) flushAudio();
        },
        finish: () => {
          flushAudio();
          if (ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ realtimeInput: { audioStreamEnd: true } }));
          }
          // Audio sent just before stop may not have produced a VAD/interim
          // event yet; give the server a moment before deciding nothing is
          // pending. A final transcript closes sooner.
          setTimeout(finishIfDone, GEMINI_IDLE_GRACE_MS);
        },
      };
      ws.onopen = () => {
        if (wsRef.current !== ws || genRef.current !== gen) return;
        // Must match the config locked into the token server-side.
        const transcription: Record<string, unknown> = {};
        if (minted.language) transcription.languageCodes = [minted.language];
        ws.send(
          JSON.stringify({
            setup: {
              model: `models/${minted.model ?? "gemini-3.5-transcribe-live"}`,
              generationConfig: { responseModalities: ["TEXT"] },
              inputAudioTranscription: transcription,
            },
          }),
        );
      };
      ws.onmessage = (ev) => {
        if (wsRef.current !== ws || genRef.current !== gen) return;
        let msg: GeminiServerMessage;
        try {
          const raw = typeof ev.data === "string" ? ev.data : new TextDecoder().decode(ev.data as ArrayBuffer);
          msg = JSON.parse(raw);
        } catch {
          return;
        }
        if (msg.setupComplete) {
          readyRef.current = true;
          setState("listening");
        }
        const va = msg.voiceActivity?.type;
        if (va === "ACTIVITY_START") speaking = true;
        const content = msg.serverContent;
        if (content?.interimInputTranscription) {
          const text = (content.interimInputTranscription.text ?? "").trim();
          if (text && !finishingRef.current) {
            pendingInterimRef.current = true;
            setInterimText(text);
          } else if (text) {
            pendingInterimRef.current = true;
          }
        }
        if (msg.error) {
          fail(msg.error.message || "Transcription error.");
          return;
        }
        if (content?.inputTranscription) {
          commit((content.inputTranscription.text ?? "").trim());
          speaking = false;
          finishIfDone();
        }
        // ACTIVITY_END does not clear `speaking`: the utterance's final
        // transcript is not ordered relative to it, so stop() keeps waiting
        // for inputTranscription (or the FLUSH_TIMEOUT_MS safety net).
      };
    } else {
      transportRef.current = {
        sendAudio: (buf) => {
          if (ws.readyState === WebSocket.OPEN) ws.send(buf);
        },
        finish: () => {
          if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "audio.done" }));
        },
      };
      ws.onmessage = (ev) => {
        // Ignore messages from a socket that a newer session has superseded.
        if (wsRef.current !== ws || genRef.current !== gen) return;
        let msg: TranscriptEvent;
        try {
          msg = JSON.parse(typeof ev.data === "string" ? ev.data : "");
        } catch {
          return;
        }
        switch (msg.type) {
          case "transcript.created":
            readyRef.current = true;
            setState("listening");
            break;
          case "transcript.partial": {
            const text = (msg.text ?? "").trim();
            if (!msg.is_final) {
              pendingInterimRef.current = true;
              setInterimText(text);
            } else if (msg.speech_final) {
              // Complete stitched utterance — commit and clear interim.
              commit(text);
            } else {
              // Chunk final: locked but not the end of the utterance. Keep it
              // visible as interim; the utterance-final event supersedes it.
              pendingInterimRef.current = true;
              setInterimText(text);
            }
            break;
          }
          case "transcript.done": {
            // Final flush after audio.done — append the trailing transcript
            // only if an utterance was still pending (not already committed via
            // a speech_final event), so we never double-append.
            const text = (msg.text ?? "").trim();
            if (text && pendingInterimRef.current && text !== lastCommittedRef.current) {
              onFinalRef.current(text);
            }
            pendingInterimRef.current = false;
            setInterimText("");
            setState("idle");
            teardown(true);
            break;
          }
          case "error":
            fail(msg.message || "Transcription error.");
            break;
        }
      };
    }

    // 4. Wire the audio graph. Frames are only sent once the server signals
    //    readiness (transcript.created / setupComplete), gated by readyRef.
    try {
      const ctx = new AudioContext();
      ctxRef.current = ctx;
      const source = ctx.createMediaStreamSource(stream);
      sourceRef.current = source;

      const send = (buf: ArrayBuffer) => {
        if (readyRef.current && !finishingRef.current) transportRef.current?.sendAudio(buf);
      };

      let usedWorklet = false;
      if (ctx.audioWorklet) {
        try {
          await ctx.audioWorklet.addModule("/stt-worklet.js");
          if (genRef.current !== gen) return; // superseded during module load
          const node = new AudioWorkletNode(ctx, "stt-processor");
          node.port.onmessage = (e: MessageEvent) => send(e.data as ArrayBuffer);
          source.connect(node);
          // Keep the node pulling without routing mic audio to the speakers.
          node.connect(ctx.destination);
          nodeRef.current = node;
          usedWorklet = true;
        } catch {
          usedWorklet = false;
        }
      }
      if (!usedWorklet) {
        // Fallback: ScriptProcessorNode (deprecated but broadly available).
        const node = ctx.createScriptProcessor(4096, 1, 1);
        const ratio = ctx.sampleRate / TARGET_RATE;
        // Continuous fractional read index carried across callbacks so the
        // decimation stays phase-accurate (no per-buffer reset / drift).
        let pos = 0;
        node.onaudioprocess = (e: AudioProcessingEvent) => {
          const input = e.inputBuffer.getChannelData(0);
          const len = input.length;
          const out: number[] = [];
          for (; pos < len; pos += ratio) {
            let s = input[Math.floor(pos)];
            if (s > 1) s = 1;
            else if (s < -1) s = -1;
            out.push(s < 0 ? s * 0x8000 : s * 0x7fff);
          }
          pos -= len;
          if (out.length > 0) send(Int16Array.from(out).buffer);
        };
        source.connect(node);
        node.connect(ctx.destination);
        nodeRef.current = node;
      }
    } catch (err) {
      if (genRef.current !== gen) return;
      fail(errMsg(err));
      return;
    }
  }, [supported, fail, teardown]);

  const stop = useCallback(() => {
    startSeqRef.current++;
    if (finishingRef.current) {
      // Already flushing (or a start() is waiting on the flush): cancel the
      // pending start but let the final transcript land.
      setState("idle");
      return;
    }
    const ws = wsRef.current;
    // Stop capturing immediately, but keep the socket open long enough to
    // flush the last utterance: ask the provider to finish and let its final
    // transcript (or the close handler) complete teardown. Do NOT bump the
    // generation yet, so the socket handlers stay live to receive it.
    const wasReady = readyRef.current;
    readyRef.current = false;
    if (nodeRef.current && "port" in nodeRef.current) {
      (nodeRef.current as AudioWorkletNode).port.onmessage = null;
    }
    try {
      nodeRef.current?.disconnect();
      sourceRef.current?.disconnect();
    } catch {
      /* already gone */
    }
    streamRef.current?.getTracks().forEach((t) => t.stop());
    const transport = transportRef.current;
    if (ws && ws.readyState === WebSocket.OPEN && wasReady && transport) {
      finishingRef.current = true;
      let resolve = () => {};
      const promise = new Promise<void>((r) => {
        resolve = r;
      });
      finishWaitRef.current = { promise, resolve };
      setState("idle");
      try {
        transport.finish();
      } catch {
        /* ignore */
      }
      // Safety net: if the final transcript never arrives, force-close.
      setTimeout(() => {
        if (wsRef.current === ws) teardown(true);
      }, FLUSH_TIMEOUT_MS);
    } else {
      teardown(true);
      setState("idle");
    }
    setInterimText("");
  }, [teardown]);

  // Clean up on unmount.
  useEffect(
    () => () => {
      startSeqRef.current++; // abort a start() waiting on a flush
      teardown(true);
    },
    [teardown],
  );

  return { state, interimText, error, supported, start, stop };
}
