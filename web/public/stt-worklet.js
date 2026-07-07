// AudioWorklet processor for streaming Speech-to-Text.
//
// Receives mono float32 audio at the AudioContext's native sample rate,
// downsamples to 16 kHz, converts to signed 16-bit little-endian PCM, and
// posts each chunk back to the main thread as a transferable ArrayBuffer.
// The main thread forwards these as binary WebSocket frames to the xAI STT
// endpoint (encoding=pcm, sample_rate=16000).
//
// Downsampling is a simple decimating average — adequate for speech STT and
// cheap enough to run in the audio render thread without underruns.

const TARGET_RATE = 16000;

class STTProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    // Continuous fractional read index into the current input buffer. The
    // leftover (pos - bufferLength) is carried into the next render quantum
    // so the decimation ratio stays exact over time (no phase reset / drift).
    this._pos = 0;
  }

  process(inputs) {
    const input = inputs[0];
    if (!input || input.length === 0) return true;
    const channel = input[0];
    if (!channel || channel.length === 0) return true;

    const len = channel.length;
    const ratio = sampleRate / TARGET_RATE; // sampleRate is a worklet-scope global
    const out = [];
    let pos = this._pos;
    for (; pos < len; pos += ratio) {
      let s = channel[Math.floor(pos)];
      if (s > 1) s = 1;
      else if (s < -1) s = -1;
      out.push(s < 0 ? s * 0x8000 : s * 0x7fff);
    }
    // Carry the fractional remainder (0..ratio) into the next quantum.
    this._pos = pos - len;

    if (out.length > 0) {
      const buf = Int16Array.from(out);
      this.port.postMessage(buf.buffer, [buf.buffer]);
    }
    return true;
  }
}

registerProcessor("stt-processor", STTProcessor);
