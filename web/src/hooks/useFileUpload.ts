import { useCallback, useRef, useState } from "react";
import type { AgentMessageAttachment } from "../lib/agentApi";
import { api } from "../lib/api";

const UPLOAD_FAILED = "Upload failed: ";

/**
 * Owns the composer's file-attachment lifecycle: a hidden <input> ref, the
 * pending (uploaded but not-yet-sent) attachments, and the upload/remove
 * handlers. Uploads run concurrently — including overlapping picker batches:
 * `uploading` stays true until every in-flight batch settles, so the composer
 * can keep the picker enabled and gate only the send. Partial failures
 * surface as an error string while successful uploads still attach.
 *
 * The upload target is `peerId` (the holder peer uploads go to) plus an
 * optional `scope` (the conversation). Changing either starts a new
 * generation: pending files and the error reset, batches still in flight
 * for the old target no longer count toward `uploading`, and their results
 * are discarded when they land.
 */
export function useFileUpload(peerId?: string, scope?: string) {
  const [pendingFiles, setPendingFiles] = useState<AgentMessageAttachment[]>([]);
  const [uploadError, setUploadError] = useState<string | null>(null);
  // In-flight picker batches per generation.
  const [inFlight, setInFlight] = useState<Record<number, number>>({});
  const targetKey = `${scope ?? ""}\u0000${peerId ?? ""}`;
  const [target, setTarget] = useState({ key: targetKey, gen: 0 });
  if (target.key !== targetKey) {
    // Adjust during render (not in an effect) so the first paint for the
    // new target already shows its own pending/busy state.
    setTarget({ key: targetKey, gen: target.gen + 1 });
    setPendingFiles([]);
    setUploadError(null);
  }
  const genRef = useRef(target.gen);
  genRef.current = target.gen;
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleFileSelect = useCallback(async (e: React.ChangeEvent<HTMLInputElement>) => {
    // Snapshot the live FileList before resetting the input: clearing
    // `value` empties it, and the reset has to happen now (not after the
    // upload) so the same file can be re-picked while this batch runs.
    const files = e.target.files ? Array.from(e.target.files) : [];
    e.target.value = "";
    if (files.length === 0) return;
    const gen = genRef.current;
    setInFlight((m) => ({ ...m, [gen]: (m[gen] ?? 0) + 1 }));
    setUploadError(null);
    try {
      const results = await Promise.allSettled(
        files.map((file) => api.upload(file, peerId)),
      );
      const uploaded: AgentMessageAttachment[] = [];
      const failed: string[] = [];
      for (let i = 0; i < results.length; i++) {
        const r = results[i];
        if (r.status === "fulfilled") {
          uploaded.push({ path: r.value.path, name: r.value.name, size: r.value.size, mime: r.value.mime, peerId: r.value.peerId });
        } else {
          failed.push(files[i].name);
        }
      }
      // A handoff (or conversation switch) can finish while uploads are in
      // flight. Paths returned for the old target must never be attached to
      // a turn on the new one.
      if (genRef.current !== gen) return;
      if (uploaded.length > 0) {
        setPendingFiles((prev) => [...prev, ...uploaded]);
      }
      if (failed.length > 0) {
        // Overlapping batches: append to a still-showing failure from an
        // earlier batch instead of replacing it.
        setUploadError((prev) =>
          prev?.startsWith(UPLOAD_FAILED)
            ? `${prev}, ${failed.join(", ")}`
            : `${UPLOAD_FAILED}${failed.join(", ")}`,
        );
      }
    } finally {
      setInFlight((m) => {
        const { [gen]: n = 0, ...rest } = m;
        return n > 1 ? { ...rest, [gen]: n - 1 } : rest;
      });
    }
  }, [peerId]);

  const removePendingFile = useCallback((index: number) => {
    setPendingFiles((prev) => prev.filter((_, i) => i !== index));
  }, []);

  return {
    pendingFiles,
    setPendingFiles,
    uploading: (inFlight[target.gen] ?? 0) > 0,
    uploadError,
    setUploadError,
    fileInputRef,
    handleFileSelect,
    removePendingFile,
  };
}
