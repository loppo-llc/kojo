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
	"time"
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

// IsAllowedImageExt returns true if ext (case-insensitive) is an accepted avatar image extension.
func IsAllowedImageExt(ext string) bool {
	return allowedImageExts[strings.ToLower(ext)]
}

// avatarMeta returns whether an avatar file exists and a modtime-derived hash.
// Single pass: one avatarFilePath lookup + one Stat.
func avatarMeta(agentID string) (exists bool, hash string) {
	p := avatarFilePath(agentID)
	if p == "" {
		return false, ""
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false, ""
	}
	return true, fmt.Sprintf("%x", fi.ModTime().UnixNano())
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

// avatarFilePath returns the path to the agent's avatar file, or "".
func avatarFilePath(agentID string) string {
	dir := agentDir(agentID)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".svg"} {
		p := filepath.Join(dir, "avatar"+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ServeAvatar serves the agent's avatar image, falling back to a generated SVG.
func ServeAvatar(w http.ResponseWriter, r *http.Request, a *Agent) {
	if path := avatarFilePath(a.ID); path != "" {
		http.ServeFile(w, r, path)
		return
	}
	// Generate SVG fallback
	svg := generateSVGAvatar(a.Name)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write([]byte(svg))
}

// SaveAvatar saves an uploaded avatar file for an agent.
func SaveAvatar(agentID string, src io.Reader, ext string) error {
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Remove any existing avatars
	for _, e := range []string{".png", ".jpg", ".jpeg", ".webp", ".svg"} {
		os.Remove(filepath.Join(dir, "avatar"+e))
	}

	dst, err := os.Create(filepath.Join(dir, "avatar"+ext))
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
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
