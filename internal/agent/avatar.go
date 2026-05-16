package agent

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
)

// Sentinel errors for ValidateTempAvatarPath, allowing callers to map to
// appropriate HTTP status codes.
var (
	ErrAvatarInternal         = errors.New("cannot resolve temp dir")
	ErrAvatarNotFound         = errors.New("file not found")
	ErrAvatarUnsupportedImage = errors.New("unsupported image format")
)

// allowedImageExts is the set of image extensions accepted for avatars.
var allowedImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".svg": true,
}

// avatarExtProbe is the set of extensions probed (and cleaned) when
// resolving an agent's stored avatar. Order is the same as the legacy
// disk path: an agent that has both `avatar.png` and `avatar.svg`
// (legitimately disallowed by SaveAvatar but possible from a
// hand-edited blob tree) presents the .png first to keep behaviour
// stable across the cutover. The list is also used by SaveAvatar to
// know what to delete before publishing the new extension.
var avatarExtProbe = []string{".png", ".jpg", ".jpeg", ".webp", ".svg"}

// avatarMu serializes avatar operations per agent.
//
// Writers (SaveAvatar / DeleteAvatar) take Lock() so the
// "delete other extensions, then Put" sequence is observed
// atomically by concurrent callers — without this two parallel
// uploads at different extensions could interleave their delete and
// put calls and end up with multiple avatar rows surviving, with
// resolveAvatarBlob's probe order silently picking one and shadowing
// the other.
//
// Readers (ServeAvatar) take RLock() so they observe a consistent
// pair of (file body, blob_refs ETag/ModTime). blob.Store.Put
// internally rename's the temp file before updating the blob_refs
// row, so without this serialization a concurrent ServeAvatar could
// open the new body but read the old refs row's digest — sending
// the client a Content-Length / SHA mismatch. RLock allows multiple
// concurrent reads (the common case is many tabs hitting GET
// /agents/<id>/avatar) while a writer is exclusive.
//
// Lock entries are process-local and intentionally NEVER reclaimed:
// removing an entry while a goroutine waits on the old mutex would
// silently let a new entry be created and break the serialization
// invariant. The leak is bounded by total agent ids ever created
// (one *sync.RWMutex per id, ~24 bytes), matching the same pattern
// used by Manager.patchMus.
var (
	avatarMuMu sync.Mutex
	avatarMu   = map[string]*sync.RWMutex{}
)

func avatarLockFor(agentID string) *sync.RWMutex {
	avatarMuMu.Lock()
	mu, ok := avatarMu[agentID]
	if !ok {
		mu = &sync.RWMutex{}
		avatarMu[agentID] = mu
	}
	avatarMuMu.Unlock()
	return mu
}

func acquireAvatarLock(agentID string) func() {
	mu := avatarLockFor(agentID)
	mu.Lock()
	return mu.Unlock
}

func acquireAvatarRLock(agentID string) func() {
	mu := avatarLockFor(agentID)
	mu.RLock()
	return mu.RUnlock
}

// IsAllowedImageExt returns true if ext (case-insensitive) is an accepted avatar image extension.
func IsAllowedImageExt(ext string) bool {
	return allowedImageExts[strings.ToLower(ext)]
}

// avatarBlobPath returns the logical path under blob.ScopeGlobal for
// an agent's avatar at the given extension. Centralised so the URI
// scheme (`agents/<id>/avatar.<ext>`) is defined in exactly one place
// — the migration importer (internal/migrate/importers/blobs.go) and
// the runtime read/write paths must agree byte-for-byte or
// post-migration installs would silently miss their avatars.
func avatarBlobPath(agentID, ext string) string {
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return "agents/" + agentID + "/avatar" + ext
}

// resolveAvatarBlob probes blob.ScopeGlobal for the first existing
// avatar.<ext> file and returns (ext, object, ok). Returns ok=false
// if no avatar is published or the blob store is nil. The probe runs
// blob.Head per extension (Lstat + cache lookup, no body read), so
// cost is the same as the legacy os.Stat loop but with
// blob_refs-backed ETag included in the returned object.
//
// On success ext includes the leading dot ("." + e.g. "png"). The
// object's ETag is the strong "sha256:<hex>" form when blob_refs is
// wired; tests with a bare blob.Store get ETag="" which callers
// fall back to in avatarMeta.
func resolveAvatarBlob(bs *blob.Store, agentID string) (string, *blob.Object, bool) {
	if bs == nil {
		return "", nil, false
	}
	for _, ext := range avatarExtProbe {
		obj, err := bs.Head(blob.ScopeGlobal, avatarBlobPath(agentID, ext))
		if err == nil && obj != nil {
			return ext, obj, true
		}
	}
	return "", nil, false
}

// avatarMeta returns whether an avatar blob exists and a content-
// derived hash for ETag use. Cache key is the blob_refs sha256 (via
// resolveAvatarBlob) so a re-upload that produces an identical body
// also produces an identical hash — matching the design contract
// for a content-addressed blob store. When the blob store is wired
// without refs (slice-1 path / unit tests), or the row hasn't been
// backfilled, we fall back to a ModTime-derived hash so freshly
// published avatars still defeat HTTP caches.
func (m *Manager) avatarMeta(agentID string) (exists bool, hash string) {
	// Hold the read side of the avatar lock so a concurrent
	// SaveAvatar's rename → blob_refs.Put gap can't surface a
	// hash derived from the new body's ETag-from-old-row state.
	// Brief: resolveAvatarBlob calls blob.Head which reads both
	// the on-disk file (for size/modtime) and the blob_refs row
	// (for ETag); without the lock those two reads can straddle a
	// concurrent Put.
	runlock := acquireAvatarRLock(agentID)
	defer runlock()
	_, obj, ok := resolveAvatarBlob(m.blobStore, agentID)
	if !ok {
		return false, ""
	}
	if obj.ETag != "" {
		// Strip the "sha256:" prefix; the public AvatarHash field
		// has historically been a bare hex string and the Web UI
		// embeds it in the avatar URL's cache-bust query param
		// (?t=<hash> in AgentAvatar.tsx) — the bare-hex form
		// preserves backward compatibility with v0 consumers.
		return true, strings.TrimPrefix(obj.ETag, "sha256:")
	}
	return true, fmt.Sprintf("%x", obj.ModTime)
}

// applyAvatarMeta sets HasAvatar and AvatarHash on the agent from pre-fetched values.
// Falls back to UpdatedAt as hash when no avatar exists.
// Call avatarMeta(id) outside any lock to get has/hash, then apply under lock.
func applyAvatarMeta(a *Agent, has bool, hash string) {
	a.HasAvatar = has
	a.AvatarHash = hash
	if !has {
		a.AvatarHash = a.UpdatedAt
	}
}

// ServeAvatar serves the agent's avatar image, falling back to a
// generated SVG when no avatar is published or the blob store
// hasn't been wired (test fixture). Uses http.ServeContent so
// conditional GET (If-Modified-Since / If-None-Match) and Range
// requests work the same way they did in the v0 http.ServeFile path.
//
// Content-Type is inferred from the file extension; svg is special-
// cased because Go's mime package returns "image/svg+xml" but only
// when the system mime database has been initialized, which can't
// be relied on across all deploy targets.
func ServeAvatar(bs *blob.Store, w http.ResponseWriter, r *http.Request, a *Agent) {
	// Hold the per-agent avatar read lock JUST across the resolve →
	// Open → header-snapshot window so a concurrent SaveAvatar's
	// rename → blob_refs.Put gap can't surface a fresh body with a
	// stale ETag. The lock is released BEFORE http.ServeContent
	// streams the body — once we have an open *os.File the kernel
	// holds the inode, so a subsequent SaveAvatar that rename's a
	// different file into the same path doesn't disturb our stream.
	// Holding the read lock across the body write would let a slow
	// HTTP client (mobile uplink, attacker-throttled) starve
	// SaveAvatar / DeleteAvatar.
	f, ext, etag, modTime, ok := openAvatarForServe(bs, a.ID)
	if ok {
		defer f.Close()
		ctype := contentTypeForAvatarExt(ext)
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		if etag != "" {
			w.Header().Set("ETag", `"`+etag+`"`)
		}
		http.ServeContent(w, r, "avatar"+ext, modTime, f)
		return
	}
	// Fall through to SVG fallback on either no-avatar OR Open
	// failure — a missing blob between Head and Open means a
	// concurrent SaveAvatar/DeleteAvatar dropped the row, and
	// serving the legacy initials avatar is preferable to a 500.
	svg := generateSVGAvatar(a.Name)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write([]byte(svg))
}

// openAvatarForServe is ServeAvatar's lock-scoped helper: it takes
// the per-agent avatar read lock, resolves the published extension,
// opens the blob, snapshots the Object's ETag/ModTime, and returns
// the open file handle to the caller. The read lock is released on
// return so the caller can stream the body without blocking writers.
//
// Returning an open *os.File past the lock release is safe: blob.
// Store.Put never truncates in place — it writes a temp file and
// rename's it onto the target — so an inode our caller is reading
// stays valid even after a concurrent re-Put removes the directory
// entry. Closing the returned handle is the caller's responsibility.
func openAvatarForServe(bs *blob.Store, agentID string) (f *os.File, ext, etag string, modTime time.Time, ok bool) {
	runlock := acquireAvatarRLock(agentID)
	defer runlock()
	ext, _, found := resolveAvatarBlob(bs, agentID)
	if !found {
		return nil, "", "", time.Time{}, false
	}
	f, obj, err := bs.Open(blob.ScopeGlobal, avatarBlobPath(agentID, ext))
	if err != nil {
		return nil, "", "", time.Time{}, false
	}
	return f, ext, obj.ETag, time.UnixMilli(obj.ModTime), true
}

// contentTypeForAvatarExt maps an avatar extension to its MIME type.
// Defined here so ServeAvatar doesn't depend on the host's
// mime.types database (Go's mime.TypeByExtension reads /etc/mime.types
// on Linux, which a minimal container image may not ship).
func contentTypeForAvatarExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	}
	return ""
}

// SaveAvatar publishes an uploaded avatar to the blob store. ext is
// the leading-dot extension ("." + e.g. "png"); callers are
// responsible for validating it via IsAllowedImageExt before calling.
//
// Removes any pre-existing avatar at a different extension so the
// agent presents exactly one avatar at a time — without this a user
// who first uploads avatar.png and then avatar.svg would have BOTH
// surface, with resolveAvatarBlob's probe order picking .png and
// silently discarding the new svg. Matches the v0 disk-write
// semantics: SaveAvatar deletes every avatar.* before writing the
// new one.
//
// Per-agent serialization (acquireAvatarLock) ensures the
// "delete-then-put" sequence is observed atomically — without it,
// two concurrent uploads at different extensions could interleave
// their delete/put calls and leave multiple rows in place.
//
// Failure posture: if ANY of the per-extension cleanup Deletes
// fails (other than ErrNotFound), SaveAvatar aborts before the
// final Put. This is intentional — a partial cleanup that
// proceeds to Put could leave two avatars surviving and the wrong
// one wins resolveAvatarBlob's probe order. Aborting forces the
// operator to see and resolve the underlying store error.
func SaveAvatar(bs *blob.Store, agentID string, src io.Reader, ext string) error {
	if bs == nil {
		return errors.New("avatar: blob store not configured")
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	unlock := acquireAvatarLock(agentID)
	defer unlock()

	// Delete every other extension first. Aborting on a non-
	// ErrNotFound error preserves the "exactly one avatar" invariant
	// — see the failure-posture note in the function's doc above.
	for _, e := range avatarExtProbe {
		if e == ext {
			continue
		}
		if err := bs.Delete(blob.ScopeGlobal, avatarBlobPath(agentID, e), blob.DeleteOptions{}); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("avatar: cleanup old %s: %w", e, err)
		}
	}

	if _, err := bs.Put(blob.ScopeGlobal, avatarBlobPath(agentID, ext), src, blob.PutOptions{}); err != nil {
		return fmt.Errorf("avatar: put: %w", err)
	}
	return nil
}

// DeleteAvatar removes every published avatar.* blob for an agent.
// Used by the reset and delete paths so a re-created agent sharing
// the same id starts with no avatar. ErrNotFound on individual
// extensions is folded into success — the post-condition is "no
// avatar blobs for this agent", which is already true for any
// extension that wasn't published.
//
// Acquires the per-agent avatar lock so a concurrent SaveAvatar
// can't observe a half-deleted state (some extensions cleaned, the
// new Put landed). See SaveAvatar for the rationale.
func DeleteAvatar(bs *blob.Store, agentID string) error {
	if bs == nil {
		return nil
	}
	unlock := acquireAvatarLock(agentID)
	defer unlock()
	for _, e := range avatarExtProbe {
		if err := bs.Delete(blob.ScopeGlobal, avatarBlobPath(agentID, e), blob.DeleteOptions{}); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("avatar: delete %s: %w", e, err)
		}
	}
	return nil
}

// ValidateTempAvatarPath validates that a path points to an image file inside
// a kojo-avatar-* temp directory. Returns the resolved absolute path or an error.
// Used by handlers that accept user-supplied avatar paths.
func ValidateTempAvatarPath(avatarPath string) (string, error) {
	absPath, err := filepath.EvalSymlinks(avatarPath)
	if err != nil {
		return "", fmt.Errorf("invalid avatar path")
	}
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return "", ErrAvatarInternal
	}
	if !strings.HasPrefix(absPath, tempDir+string(filepath.Separator)) {
		return "", fmt.Errorf("avatar path must be in temp directory")
	}
	rel, _ := filepath.Rel(tempDir, absPath)
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "kojo-avatar-") {
		return "", fmt.Errorf("invalid avatar path")
	}
	ext := strings.ToLower(filepath.Ext(absPath))
	if !IsAllowedImageExt(ext) {
		return "", ErrAvatarUnsupportedImage
	}
	fi, err := os.Stat(absPath)
	if err != nil || !fi.Mode().IsRegular() {
		return "", ErrAvatarNotFound
	}
	return absPath, nil
}

// geminiImageModel is the default image-output Gemini model.
// Override via KOJO_GEMINI_IMAGE_MODEL.
const geminiImageModel = "gemini-3.1-flash-image-preview"

// resolveGeminiAPIKey returns an API key from env vars or legacy
// nanobanana credentials file, or "" if none is configured.
func resolveGeminiAPIKey() string {
	for _, v := range []string{"KOJO_GEMINI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if key := strings.TrimSpace(os.Getenv(v)); key != "" {
			return key
		}
	}
	credPath := filepath.Join(os.Getenv("HOME"), ".config", "nanobanana", "credentials")
	if b, err := os.ReadFile(credPath); err == nil {
		if key := strings.TrimSpace(string(b)); key != "" {
			return key
		}
	}
	return ""
}

func extFromMimeType(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

// GenerateAvatarWithAI generates an avatar by calling the Gemini image
// generation API directly. Returns the path to an image file inside a
// kojo-avatar-* temp dir.
func GenerateAvatarWithAI(ctx context.Context, agentID string, persona string, name string, prompt string, logger *slog.Logger) (string, error) {
	apiKey := resolveGeminiAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("no Gemini API key configured (set GEMINI_API_KEY)")
	}

	// Build image generation prompt
	imagePrompt := fmt.Sprintf("Character portrait avatar of %s, ", name)
	if persona != "" {
		runes := []rune(persona)
		if len(runes) > 100 {
			runes = runes[:100]
		}
		imagePrompt += string(runes) + ", "
	}
	if prompt != "" {
		imagePrompt += prompt + ", "
	}
	imagePrompt += "flat illustration style, clean background, centered face, square format"

	model := geminiImageModel
	if m := strings.TrimSpace(os.Getenv("KOJO_GEMINI_IMAGE_MODEL")); m != "" {
		model = m
	}

	reqBody := map[string]any{
		"contents": []any{
			map[string]any{
				"parts": []any{
					map[string]any{"text": imagePrompt},
				},
			},
		},
		"generationConfig": map[string]any{
			"responseModalities": []string{"IMAGE"},
			"imageConfig": map[string]any{
				"aspectRatio": "1:1",
				"imageSize":   "1K",
			},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	reqCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", apiKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		msg := apiErr.Error.Message
		if msg == "" {
			msg = string(body)
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		logger.Debug("gemini API error", "status", resp.StatusCode, "body", string(body))
		return "", fmt.Errorf("gemini API HTTP %d: %s", resp.StatusCode, msg)
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Candidates) == 0 {
		if parsed.PromptFeedback.BlockReason != "" {
			return "", fmt.Errorf("gemini blocked: %s", parsed.PromptFeedback.BlockReason)
		}
		return "", fmt.Errorf("no candidates in response")
	}

	var mimeType, b64 string
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.InlineData != nil && p.InlineData.Data != "" {
			mimeType = p.InlineData.MimeType
			b64 = p.InlineData.Data
			break
		}
	}
	if b64 == "" {
		return "", fmt.Errorf("no image data in response")
	}
	ext := extFromMimeType(mimeType)
	if ext == "" {
		return "", fmt.Errorf("unsupported mime type: %s", mimeType)
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "kojo-avatar-*")
	if err != nil {
		return "", err
	}
	outPath := filepath.Join(tmpDir, "avatar"+ext)
	if err := os.WriteFile(outPath, raw, 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("write image: %w", err)
	}
	return outPath, nil
}

// GenerateSVGAvatarFile creates an SVG avatar file in a temp directory and returns its path.
// Used as fallback when AI avatar generation is unavailable.
func GenerateSVGAvatarFile(name string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "kojo-avatar-*")
	if err != nil {
		return "", err
	}
	svg := generateSVGAvatar(name)
	p := filepath.Join(tmpDir, "avatar.svg")
	if err := os.WriteFile(p, []byte(svg), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// generateSVGAvatar creates a fallback SVG avatar using name-derived gradient and initials.
func generateSVGAvatar(name string) string {
	hash := md5.Sum([]byte(name))

	// Generate two colors from hash for gradient
	h1 := int(hash[0])%360
	h2 := (h1 + 60 + int(hash[1])%120) % 360

	// Get initials (first letter of first two words, or first two letters)
	initials := "?"
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		initials = strings.ToUpper(string([]rune(parts[0])[0:1]) + string([]rune(parts[1])[0:1]))
	} else if len(name) > 0 {
		runes := []rune(name)
		if len(runes) >= 2 {
			initials = strings.ToUpper(string(runes[0:2]))
		} else {
			initials = strings.ToUpper(string(runes[0:1]))
		}
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <defs>
    <linearGradient id="g" x1="0%%" y1="0%%" x2="100%%" y2="100%%">
      <stop offset="0%%" style="stop-color:hsl(%d,70%%,50%%)" />
      <stop offset="100%%" style="stop-color:hsl(%d,70%%,40%%)" />
    </linearGradient>
  </defs>
  <rect width="100" height="100" rx="20" fill="url(#g)" />
  <text x="50" y="50" text-anchor="middle" dominant-baseline="central"
    font-family="system-ui,-apple-system,sans-serif" font-size="36" font-weight="600" fill="white">%s</text>
</svg>`, h1, h2, initials)
}
