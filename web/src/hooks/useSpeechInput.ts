import { useCallback, useEffect, useRef, useState } from "react";
import { agentApi } from "../lib/agentApi";
import { errMsg } from "../lib/utils";

// Streaming push-to-talk speech-to-text against the xAI STT WebSocket.
//
// Flow: acquire the mic → mint an ephemeral token from our backend → open the
// xAI streaming WS authenticated via the `xai-client-secret.<token>`
// subprotocol (browsers cannot set the Authorization header on a WebSocket) →
// wait for the `transcript.created` ready event → downsample mic audio to
// 16 kHz mono PCM16 in an AudioWorklet, stream it as binary frames → surface
// interim partials live and commit finalized utterances via onFinal.
//
// Protocol reference (docs.x.ai, Streaming Speech-to-Text):
//   client → binary PCM frames; {"type":"audio.done"} to end.
//   server → transcript.created | transcript.partial | transcript.done | error
//   transcript.partial: is_final=false → interim; is_final=true,
//     speech_final=false → chunk final; both true → utterance final.

export type SpeechInputState = "idle" | "connecting" | "listening" | "error";

const TARGET_RATE = 16000;
const CLIENT_SECRET_PREFIX = "xai-client-secret.";

/** Map a browser locale (e.g. "ja-JP", "en-US") to an xAI language code. */
function localeToLanguage(locale: string): string {
  const primary = (locale || "en").toLowerCase().split("-")[0];
  return primary || "en";
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

// A minimal shape for the transcript events we consume.
interface TranscriptEvent {
  type: string;
  text?: string;
  is_final?: boolean;
  speech_final?: boolean;
  message?: string;
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

    // 2. Mint an ephemeral token from our backend.
    let token: string;
    let wsBaseUrl: string;
    try {
      const r = await agentApi.stt.token();
      token = r.token;
      wsBaseUrl = r.wsBaseUrl;
    } catch (err) {
      if (genRef.current !== gen) return;
      fail(errMsg(err));
      return;
    }
    if (genRef.current !== gen) return;

    // 3. Open the STT WebSocket (subprotocol carries the ephemeral token).
    const lang = localeToLanguage(navigator.language);
    const url =
      `${wsBaseUrl}?sample_rate=${TARGET_RATE}&encoding=pcm&interim_results=true` +
      `&language=${encodeURIComponent(lang)}`;
    let ws: WebSocket;
    try {
      ws = new WebSocket(url, [CLIENT_SECRET_PREFIX + token]);
    } catch (err) {
      fail(errMsg(err));
      return;
    }
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;

    ws.onerror = () => {
      setError((e) => e || "Voice connection error.");
    };
    ws.onclose = () => {
      // Intended closes route through teardown(), which nulls wsRef and bumps
      // the generation, so reaching here with this ws still current means the
      // server closed the socket unexpectedly (auth failure, network, token
      // expiry). Tear down the mic/AudioContext and surface an error.
      if (wsRef.current !== ws || genRef.current !== gen) return;
      wsRef.current = null;
      teardown(false);
      setInterimText("");
      setError((e) => e || "Voice connection closed unexpectedly.");
      setState("error");
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
            if (text) {
              lastCommittedRef.current = text;
              onFinalRef.current(text);
            }
            pendingInterimRef.current = false;
            setInterimText("");
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

    // 4. Wire the audio graph. Frames are only sent once the server signals
    //    readiness (transcript.created), gated by readyRef.
    try {
      const ctx = new AudioContext();
      ctxRef.current = ctx;
      const source = ctx.createMediaStreamSource(stream);
      sourceRef.current = source;

      const send = (buf: ArrayBuffer) => {
        if (readyRef.current && ws.readyState === WebSocket.OPEN) ws.send(buf);
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
    const ws = wsRef.current;
    // Stop capturing immediately, but keep the socket open long enough to
    // flush the last utterance: send audio.done and let transcript.done (or
    // the close handler) finish teardown. Do NOT bump the generation yet, so
    // the socket handlers stay live to receive the final transcript.
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
    if (ws && ws.readyState === WebSocket.OPEN) {
      try {
        ws.send(JSON.stringify({ type: "audio.done" }));
      } catch {
        /* ignore */
      }
      // Safety net: if transcript.done never arrives, force-close.
      setTimeout(() => {
        if (wsRef.current === ws) teardown(true);
      }, 3000);
      setState("idle");
    } else {
      teardown(true);
      setState("idle");
    }
    setInterimText("");
  }, [teardown]);

  // Clean up on unmount.
  useEffect(() => () => teardown(true), [teardown]);

  return { state, interimText, error, supported, start, stop };
}
