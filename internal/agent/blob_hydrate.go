package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

// hydrateAgentDirFromBlobs materializes blob_refs entries for one
// agent into agentDir(id). Used at Manager.Load so the v1 CLI process
// (cmd.Dir = agentDir) can `ls books/`, `cat outputs/result.bvh`,
// etc. just like it could on v0 — even though the canonical store is
// now the per-scope blob tree under <configdir>/{global,local}/.
//
// Skip rules (mirrors and complements internal/migrate/importers/
// blobs.go's catchall):
//   - kojo://machine/agents/<id>/ — credentials.{json,key}; the
//     runtime never reads them from agentDir (credential.go owns
//     the canonical encrypted store) and writing them to disk would
//     leak machine-bound secrets to the agent CWD where the CLI
//     could accidentally pick them up.
//   - agents/<id>/avatar.<ext> — Web UI surfaces avatars via
//     /api/v1/agents/<id>/avatar which Get's the blob direct; the
//     CLI never reads avatar from CWD.
//   - agents/<id>/index/memory.db — RAG index; the runtime
//     regenerates per-peer on first use, materializing v0's snapshot
//     would freeze a stale layout.
//
// Already-on-disk leaves whose sha256 matches the blob_refs row are
// skipped to avoid pointless rewrites on every agent load.
//
// Best-effort per row: a single failed copy logs and continues so
// one missing-on-disk blob (orphan ref) doesn't block the hydrate of
// the rest.
func hydrateAgentDirFromBlobs(ctx context.Context, st *store.Store, bs *blob.Store, agentID string, logger *slog.Logger) error {
	if st == nil || bs == nil || agentID == "" {
		return nil
	}
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hydrate blobs: ensure agent dir: %w", err)
	}

	scopes := []struct {
		scope     blob.Scope
		uriPrefix string
	}{
		{blob.ScopeGlobal, "kojo://global/agents/" + agentID + "/"},
		{blob.ScopeLocal, "kojo://local/agents/" + agentID + "/"},
	}

	var firstErr error
	for _, s := range scopes {
		refs, err := st.ListBlobRefs(ctx, store.ListBlobRefsOptions{
			Scope:     string(s.scope),
			URIPrefix: s.uriPrefix,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list blob_refs (scope=%s): %w", s.scope, err)
			}
			if logger != nil {
				logger.Warn("hydrate blobs: list failed",
					"agent", agentID, "scope", s.scope, "err", err)
			}
			continue
		}
		for _, ref := range refs {
			encoded := strings.TrimPrefix(ref.URI, s.uriPrefix)
			if encoded == "" || encoded == ref.URI {
				// uri didn't carry the expected prefix despite the
				// SQL filter — defensive skip.
				continue
			}
			// blob_refs.uri is built via blob.BuildURI which percent-
			// encodes each path segment (spaces → %20, '#' → %23,
			// non-ASCII NFC bytes, etc). The blob layer's Get and
			// the on-disk hydrate target both expect the DECODED
			// form. Decode segment-by-segment so a literal `/` in
			// the encoded form still separates segments correctly.
			rel, derr := decodeURISegments(encoded)
			if derr != nil {
				if logger != nil {
					logger.Warn("hydrate blobs: invalid percent-encoded URI",
						"agent", agentID, "uri", ref.URI, "err", derr)
				}
				if firstErr == nil {
					firstErr = derr
				}
				continue
			}
			if skipBlobHydrate(rel) {
				continue
			}
			target := filepath.Join(dir, filepath.FromSlash(rel))
			if matched, err := diskMatchesSHA(target, ref.SHA256); err != nil {
				if logger != nil {
					logger.Warn("hydrate blobs: stat target failed",
						"agent", agentID, "uri", ref.URI, "err", err)
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			} else if matched {
				continue
			}
			if err := hydrateOneBlob(bs, s.scope, "agents/"+agentID+"/"+rel, target, ref.SHA256); err != nil {
				if logger != nil {
					logger.Warn("hydrate blobs: copy failed",
						"agent", agentID, "uri", ref.URI, "err", err)
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if logger != nil {
				logger.Debug("hydrate blobs: materialized",
					"agent", agentID, "uri", ref.URI, "size", ref.Size)
			}
		}
	}
	return firstErr
}

// decodeURISegments reverses the per-segment percent-encoding that
// blob.BuildURI applies. Splits on "/" (which BuildURI deliberately
// preserves between segments to keep the prefix range scan working)
// and url.PathUnescapes each segment. Any decode error surfaces so
// the caller can warn-and-skip rather than feeding a malformed path
// into blob.Get / filepath.Join.
func decodeURISegments(encoded string) (string, error) {
	parts := strings.Split(encoded, "/")
	for i, p := range parts {
		dec, err := url.PathUnescape(p)
		if err != nil {
			return "", fmt.Errorf("decode segment %q: %w", p, err)
		}
		parts[i] = dec
	}
	return strings.Join(parts, "/"), nil
}

// skipBlobHydrate reports whether a (scope, agents/<id>/<rel>) blob
// should be skipped on the hydrate path. Mirrors the importer's
// isExplicitlyPublishedLeaf for the Web-UI-only / runtime-rebuilt
// leaves so the CLI dir isn't polluted with files no one reads from
// there.
func skipBlobHydrate(rel string) bool {
	if rel == "index/memory.db" {
		return true
	}
	for _, ext := range []string{"png", "svg", "jpg", "jpeg", "webp"} {
		if rel == "avatar."+ext {
			return true
		}
	}
	return false
}

// diskMatchesSHA returns true iff a regular file at path exists and
// its sha256 hex equals expected. A non-existent file returns
// (false, nil) so the caller proceeds to write. Any other error
// (permission, mid-walk dir-where-file-expected) propagates so the
// caller can log and skip rather than overwriting.
func diskMatchesSHA(path, expected string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("hydrate target %s: not a regular file", path)
	}
	if expected == "" {
		// No expected sha; treat any existing file as matching to
		// avoid pointless rewrites. Old blob_refs rows minted
		// before sha was tracked could land here.
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == expected, nil
}

// hydrateOneBlob streams the blob at (scope, logicalPath) into
// target, verifying the on-the-wire body's sha256 matches expected
// and using a tmp+rename so a concurrent reader (the CLI process)
// never observes a half-written file.
func hydrateOneBlob(bs *blob.Store, scope blob.Scope, logicalPath, target, expectedSHA string) error {
	rc, _, err := bs.Get(scope, logicalPath)
	if err != nil {
		return fmt.Errorf("blob.Get %s: %w", logicalPath, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("ensure dir %s: %w", filepath.Dir(target), err)
	}
	// Streaming verify-and-write: hash as we write to the tmp file
	// so we don't need to buffer the whole body in memory. atomicfile
	// can't be used directly because it takes []byte; replicate its
	// CreateTemp+Rename pattern with the streamed reader.
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if _, err := io.Copy(w, rc); err != nil {
		f.Close()
		return fmt.Errorf("copy %s: %w", logicalPath, err)
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if expectedSHA != "" && got != expectedSHA {
		return fmt.Errorf("sha256 mismatch for %s: got %s want %s", logicalPath, got, expectedSHA)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	cleanup = false
	return nil
}

// hydrateAgentBlobsAtLoad runs hydrateAgentDirFromBlobs with a fresh
// 30s context per agent, mirroring the SyncAgentMemoryFromDisk
// load-time invocation pattern (Manager.Load loops agents and runs
// each sync sequentially). Best-effort: log+continue on per-agent
// failure.
func hydrateAgentBlobsAtLoad(st *store.Store, bs *blob.Store, agentID string, logger *slog.Logger) {
	if st == nil || bs == nil {
		return
	}
	ctx, cancel := dbContextWithCancel(nil, 30*time.Second)
	defer cancel()
	if err := hydrateAgentDirFromBlobs(ctx, st, bs, agentID, logger); err != nil {
		if logger != nil {
			logger.Warn("hydrate blobs at load failed", "agent", agentID, "err", err)
		}
	}
}

