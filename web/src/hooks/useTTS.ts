import { useCallback, useEffect, useRef, useState } from "react";
import { ttsApi, pickBestFormat, type TTSCapability } from "../lib/ttsApi";

const AUTO_KEY_PREFIX = "kojo:tts:auto:";

export function autoKey(agentId: string): string {
  return AUTO_KEY_PREFIX + agentId;
}

// useTTSAutoToggle persists the per-agent "auto-play on agent reply"
// preference in localStorage. Default: OFF.
//
// When agentId changes (navigating between agents) state is re-loaded
// from localStorage so we don't accidentally write the previous agent's
// toggle into the new agent's storage key. The initial-render value is
// also read from storage so the first paint is correct.
export function useTTSAutoToggle(agentId: string | undefined) {
  const readFromStorage = (id: string | undefined): boolean => {
    if (!id) return false;
    try {
      return localStorage.getItem(autoKey(id)) === "1";
    } catch {
      return false;
    }
  };
  const [auto, setAuto] = useState<boolean>(() => readFromStorage(agentId));
  // Track which agentId the current state belongs to so we can detect
  // an external switch and refuse to write the old value to the new key.
  const lastIdRef = useRef<string | undefined>(agentId);

  useEffect(() => {
    if (lastIdRef.current !== agentId) {
      lastIdRef.current = agentId;
      setAuto(readFromStorage(agentId));
      return;
    }
    if (!agentId) return;
    try {
      localStorage.setItem(autoKey(agentId), auto ? "1" : "0");
    } catch {
      /* localStorage may be unavailable in private mode — fail silent */
    }
  }, [agentId, auto]);
  return [auto, setAuto] as const;
}

// useTTSCapability fetches the server's capability descriptor once and
// caches it on the module. The capability rarely changes within a
// session so a single fetch is fine.
let capabilityCache: TTSCapability | null = null;
let capabilityPromise: Promise<TTSCapability> | null = null;

export function useTTSCapability() {
  const [cap, setCap] = useState<TTSCapability | null>(capabilityCache);
  useEffect(() => {
    if (cap) return;
    if (!capabilityPromise) {
      capabilityPromise = ttsApi.capability();
    }
    capabilityPromise
      .then((c) => {
        capabilityCache = c;
        setCap(c);
      })
      .catch(() => {
        // Leave cap as null — UI hides TTS controls.
      });
  }, [cap]);
  return cap;
}

export type PlayState = "idle" | "loading" | "playing" | "error";

// useTTSPlayer exposes a `play(messageId, text)` function plus per-message
// state. The current Audio element is cleaned up on unmount so navigating
// away doesn't leak a still-playing track.
export function useTTSPlayer(agentId: string | undefined, enabled: boolean) {
  const cap = useTTSCapability();
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const [state, setState] = useState<Record<string, PlayState>>({});
  const [activeId, setActiveId] = useState<string | null>(null);

  // Track the latest play call so a stale fetch (network slow, user hit
  // play on a different message) cannot transition the wrong row to
  // "playing" when its audio finally arrives.
  const generationRef = useRef(0);

  useEffect(
    () => () => {
      // Bumping the generation guard cancels any in-flight synthesize
      // promise — when its `then` finally runs it'll see the mismatch
      // and bail out before creating an Audio element on an unmounted
      // component.
      generationRef.current++;
      const a = audioRef.current;
      if (a) {
        a.pause();
        a.src = "";
        audioRef.current = null;
      }
    },
    [],
  );

  const stop = useCallback(() => {
    const a = audioRef.current;
    if (a) {
      a.pause();
      a.src = "";
    }
    audioRef.current = null;
    setActiveId(null);
  }, []);

  const play = useCallback(
    async (messageId: string, text: string) => {
      if (!agentId || !enabled || !cap) return;
      // Toggle: clicking the currently-playing row stops it.
      if (activeId === messageId && audioRef.current) {
        stop();
        setState((s) => ({ ...s, [messageId]: "idle" }));
        return;
      }
      const myGen = ++generationRef.current;
      setState((s) => ({ ...s, [messageId]: "loading" }));
      stop();

      try {
        const fmt = pickBestFormat(cap.formats);
        const res = await ttsApi.synthesize(agentId, text, fmt);
        if (myGen !== generationRef.current) return; // superseded
        const audio = new Audio(ttsApi.audioUrl(res.url));
        audio.preload = "auto";
        audio.onended = () => {
          if (myGen !== generationRef.current) return;
          setState((s) => ({ ...s, [messageId]: "idle" }));
          setActiveId((cur) => (cur === messageId ? null : cur));
        };
        audio.onerror = () => {
          if (myGen !== generationRef.current) return;
          setState((s) => ({ ...s, [messageId]: "error" }));
          setActiveId((cur) => (cur === messageId ? null : cur));
        };
        audioRef.current = audio;
        setActiveId(messageId);
        setState((s) => ({ ...s, [messageId]: "playing" }));
        await audio.play();
      } catch {
        if (myGen !== generationRef.current) return;
        setState((s) => ({ ...s, [messageId]: "error" }));
        setActiveId((cur) => (cur === messageId ? null : cur));
      }
    },
    [agentId, enabled, cap, activeId, stop],
  );

  return {
    play,
    stop,
    state,
    activeId,
    capability: cap,
  };
}
