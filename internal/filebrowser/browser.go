package filebrowser

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
	"github.com/loppo-llc/kojo/internal/thumbnail"
)

// Sentinel errors for file browser operations.
var (
	ErrUnsupportedFile = errors.New("unsupported file type")
	ErrFileTooLarge    = errors.New("file too large")
	// ErrETagMismatch is returned by WriteText when the file on disk no
	// longer matches the etag the editor loaded (someone — typically the
	// agent itself — wrote it in between).
	ErrETagMismatch = errors.New("file changed on disk")
)

const maxFileSize = 1024 * 1024 // 1MB

var imageExts = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

var langExts = map[string]string{
	".go":    "go",
	".js":    "javascript",
	".jsx":   "javascript",
	".ts":    "typescript",
	".tsx":   "typescript",
	".py":    "python",
	".rs":    "rust",
	".rb":    "ruby",
	".java":  "java",
	".c":     "c",
	".cpp":   "cpp",
	".h":     "c",
	".css":   "css",
	".html":  "html",
	".json":  "json",
	".yaml":  "yaml",
	".yml":   "yaml",
	".toml":  "toml",
	".md":    "markdown",
	".sh":    "bash",
	".bash":  "bash",
	".zsh":   "bash",
	".sql":   "sql",
	".xml":   "xml",
	".swift": "swift",
	".kt":    "kotlin",
	".mod":   "go",
	".sum":   "text",
}

type Browser struct {
	logger *slog.Logger
	// writeMu serialises WriteText so two editor saves can't interleave
	// their etag check and rename. Writers outside kojo (the agent CLI)
	// are not covered; the etag check still catches them unless they land
	// inside the check→rename window.
	writeMu sync.Mutex
}

func New(logger *slog.Logger) *Browser {
	return &Browser{logger: logger}
}

type DirEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "dir" or "file"
	Size    int64  `json:"size"` // 0 for directories
	ModTime string `json:"modTime"`
}

type ListResult struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
}

// expandHome replaces a leading "~/" (or "~" alone) with the user's home
// directory. Other "~"-prefixed forms like "~user/..." are rejected rather
// than left to resolve against the CWD.
func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if path != "~" && path[1] != '/' && path[1] != filepath.Separator {
		return "", fmt.Errorf("invalid path: ~user/ form is not supported")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot determine home directory")
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func (b *Browser) List(dir string, hidden bool) (*ListResult, error) {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = home
	}

	dir, err := b.resolveValidated(dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read directory: %w", err)
	}

	result := &ListResult{
		Path:    dir,
		Entries: make([]DirEntry, 0, len(entries)),
	}

	for _, e := range entries {
		if !hidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		entryType := "file"
		if e.IsDir() {
			entryType = "dir"
		}
		info, _ := e.Info()
		modTime := time.Time{}
		var size int64
		if info != nil {
			modTime = info.ModTime()
			if !info.IsDir() {
				size = info.Size()
			}
		}
		result.Entries = append(result.Entries, DirEntry{
			Name:    e.Name(),
			Type:    entryType,
			Size:    size,
			ModTime: modTime.Format(time.RFC3339),
		})
	}

	return result, nil
}

type FileView struct {
	Path     string `json:"path"`
	Type     string `json:"type"` // "text" or "image"
	Content  string `json:"content,omitempty"`
	Language string `json:"language,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Size     int64  `json:"size"`
	URL      string `json:"url,omitempty"`
	// ETag identifies the text content the viewer loaded; the editor
	// sends it back as If-Match so a save never clobbers a newer write.
	ETag string `json:"etag,omitempty"`
}

// ContentETag returns the etag for a text file body.
func ContentETag(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:16])
}

func (b *Browser) View(path string) (*FileView, error) {
	path, err := b.resolveValidated(path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("file not found: %w", err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("path is a directory")
	}

	ext := strings.ToLower(filepath.Ext(path))

	// image
	if mime, ok := imageExts[ext]; ok {
		return &FileView{
			Path: path,
			Type: "image",
			Mime: mime,
			Size: info.Size(),
			URL:  "/api/v1/files/raw?path=" + url.QueryEscape(path),
		}, nil
	}

	// text
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, info.Size(), maxFileSize)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %w", err)
	}

	// check if binary
	if isBinary(content) {
		return nil, fmt.Errorf("%w: binary", ErrUnsupportedFile)
	}

	lang := langExts[ext]

	return &FileView{
		Path:     path,
		Type:     "text",
		Content:  string(content),
		Language: lang,
		Size:     info.Size(),
		ETag:     ContentETag(content),
	}, nil
}

// WriteText overwrites an existing text file with content, but only when
// the file still matches ifMatch (an etag from View). It is the editor's
// save path, so it deliberately refuses anything the viewer would not
// have shown as text: missing files, directories, non-regular files,
// images, binaries and files over the size cap. Symlinks are followed and
// the resolved target is replaced (atomically, keeping its permission
// bits) so the link itself survives.
func (b *Browser) WriteText(path, content, ifMatch string) (*FileView, error) {
	if ifMatch == "" {
		return nil, fmt.Errorf("etag is required")
	}
	if len(content) > maxFileSize {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, len(content), maxFileSize)
	}
	data := []byte(content)
	if isBinary(data) {
		return nil, fmt.Errorf("%w: content contains NUL bytes", ErrUnsupportedFile)
	}
	absPath, err := b.resolveValidated(path)
	if err != nil {
		return nil, err
	}
	target, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, fmt.Errorf("file not found: %w", err)
	}
	// Re-validate the resolved target: resolveValidated already does, but
	// keep the check next to the write so a future refactor can't drop it.
	if err := b.validatePath(target); err != nil {
		return nil, err
	}
	if _, ok := imageExts[strings.ToLower(filepath.Ext(target))]; ok {
		return nil, fmt.Errorf("%w: image", ErrUnsupportedFile)
	}

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("file not found: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrUnsupportedFile)
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, info.Size(), maxFileSize)
	}
	current, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %w", err)
	}
	if isBinary(current) {
		return nil, fmt.Errorf("%w: binary", ErrUnsupportedFile)
	}
	if ContentETag(current) != ifMatch {
		return nil, ErrETagMismatch
	}
	if err := atomicfile.WriteBytes(target, data, info.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("cannot write file: %w", err)
	}
	return &FileView{
		Path:     absPath,
		Type:     "text",
		Content:  content,
		Language: langExts[strings.ToLower(filepath.Ext(absPath))],
		Size:     int64(len(data)),
		ETag:     ContentETag(data),
	}, nil
}

// ServeRaw streams the file at path after expanding ~ and validating the
// resolved absolute path against the allowed roots. It does not write error
// responses itself: a pre-stream failure is returned as *thumbnail.HTTPError
// carrying the status/message the caller should deliver (in the server's JSON
// envelope). Returns nil once http.ServeFile has taken over the response.
func (b *Browser) ServeRaw(w http.ResponseWriter, r *http.Request, path string) error {
	path, err := expandHome(path)
	if err != nil {
		return thumbnail.NewHTTPError(http.StatusBadRequest, err.Error(), err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return thumbnail.NewHTTPError(http.StatusBadRequest, "invalid path", err)
	}
	if err := b.validatePath(absPath); err != nil {
		return thumbnail.NewHTTPError(http.StatusForbidden, err.Error(), err)
	}
	http.ServeFile(w, r, absPath)
	return nil
}

// ServeThumb serves a low-resolution JPEG thumbnail of the image at path.
// Path is validated against the allowed roots just like ServeRaw. Non-image
// extensions return 415 immediately so unsupported types don't hit the
// decoder. `size` is the requested longer-edge in pixels (clamped by the
// thumbnail package).
//
// Like ServeRaw, ServeThumb does not write error responses itself: a
// pre-stream failure is returned as *thumbnail.HTTPError so the caller can
// deliver it in the server's JSON envelope. Returns nil once http.ServeFile
// has taken over the response.
func (b *Browser) ServeThumb(w http.ResponseWriter, r *http.Request, path string, size int) error {
	path, err := expandHome(path)
	if err != nil {
		return thumbnail.NewHTTPError(http.StatusBadRequest, err.Error(), err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return thumbnail.NewHTTPError(http.StatusBadRequest, "invalid path", err)
	}
	if err := b.validatePath(absPath); err != nil {
		return thumbnail.NewHTTPError(http.StatusForbidden, err.Error(), err)
	}
	ext := strings.ToLower(filepath.Ext(absPath))
	if _, ok := imageExts[ext]; !ok || !thumbnail.IsSupportedExt(ext) {
		return thumbnail.NewHTTPError(http.StatusUnsupportedMediaType, "unsupported image format", nil)
	}
	cached, err := thumbnail.Generate(absPath, size)
	if err != nil {
		switch {
		case errors.Is(err, thumbnail.ErrUnsupportedFormat):
			return thumbnail.NewHTTPError(http.StatusUnsupportedMediaType, err.Error(), err)
		case errors.Is(err, thumbnail.ErrSourceTooLarge):
			// Source exceeds the byte/pixel budget — don't blow RAM
			// decoding it. 413 lets the client decide whether to fall
			// back to the raw image (which is then their decision to
			// pay the bandwidth).
			return thumbnail.NewHTTPError(http.StatusRequestEntityTooLarge, err.Error(), err)
		case os.IsNotExist(err):
			return thumbnail.NewHTTPError(http.StatusNotFound, "file not found", err)
		default:
			b.logger.Warn("thumbnail generation failed", "path", absPath, "err", err)
			return thumbnail.NewHTTPError(http.StatusInternalServerError, "thumbnail generation failed", err)
		}
	}
	// Generated thumbnails are content-addressed by (path, modtime, size,
	// filesize). Let the browser cache aggressively; callers that know
	// the source modtime should add a `v=<mtime>` query so an edit
	// produces a fresh URL.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// ETag = cache file base name (hex SHA-256). http.ServeFile picks
	// this up via ServeContent's If-None-Match handling, so a manual
	// reload returns 304 once the browser has the bytes.
	w.Header().Set("ETag", `"`+strings.TrimSuffix(filepath.Base(cached), ".jpg")+`"`)
	http.ServeFile(w, r, cached)
	return nil
}

// ValidatePath checks that the given path is under an allowed root directory.
func (b *Browser) ValidatePath(path string) error {
	_, err := b.resolveValidated(path)
	return err
}

// resolveValidated expands a leading ~, makes the path absolute, and checks
// it against the allowed roots (home / temp), returning the resolved
// absolute path. It is the shared preamble for the browser's read paths;
// validatePath (the containment check) and its error values are unchanged,
// so callers observe identical errors to the inline form.
func (b *Browser) resolveValidated(path string) (string, error) {
	path, err := expandHome(path)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	if err := b.validatePath(absPath); err != nil {
		return "", err
	}
	return absPath, nil
}

func (b *Browser) validatePath(path string) error {
	// resolve symlinks to prevent symlink traversal
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("access denied: cannot resolve path: %w", err)
		}
		// if file doesn't exist yet, resolve the parent
		resolved, err = filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("access denied: cannot resolve path")
		}
		resolved = filepath.Join(resolved, filepath.Base(path))
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("access denied: cannot determine home directory")
	}
	homeResolved, err := filepath.EvalSymlinks(home)
	if err != nil || homeResolved == "" {
		return fmt.Errorf("access denied: cannot resolve home directory")
	}

	// use path separator suffix to prevent /Users/loppo-evil matching /Users/loppo
	allowedRoots := []string{
		homeResolved + string(filepath.Separator),
	}
	// allow os.TempDir() (e.g. /var/folders/.../T/ on macOS)
	if tmpDir := os.TempDir(); tmpDir != "" {
		if tmpResolved, err := filepath.EvalSymlinks(tmpDir); err == nil && tmpResolved != "" {
			allowedRoots = append(allowedRoots, tmpResolved+string(filepath.Separator))
		}
	}
	// on macOS, /tmp is a symlink to /private/tmp which differs from os.TempDir()
	if runtime.GOOS == "darwin" {
		if tmpResolved, err := filepath.EvalSymlinks("/tmp"); err == nil && tmpResolved != "" {
			allowedRoots = append(allowedRoots, tmpResolved+string(filepath.Separator))
		}
	}

	for _, root := range allowedRoots {
		if strings.HasPrefix(resolved+string(filepath.Separator), root) {
			return nil
		}
	}

	return fmt.Errorf("access denied: path must be under home or temp directory")
}

func isBinary(data []byte) bool {
	// check first 512 bytes for null bytes
	check := data
	if len(check) > 512 {
		check = check[:512]
	}
	for _, b := range check {
		if b == 0 {
			return true
		}
	}
	return false
}
