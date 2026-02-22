package filebrowser

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
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
}

func New(logger *slog.Logger) *Browser {
	return &Browser{logger: logger}
}

type DirEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "dir" or "file"
	ModTime string `json:"modTime"`
}

type ListResult struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
}

func (b *Browser) List(dir string, hidden bool) (*ListResult, error) {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = home
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	if err := b.validatePath(dir); err != nil {
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
		if info != nil {
			modTime = info.ModTime()
		}
		result.Entries = append(result.Entries, DirEntry{
			Name:    e.Name(),
			Type:    entryType,
			ModTime: modTime.UTC().Format(time.RFC3339),
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
}

func (b *Browser) View(path string) (*FileView, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	if err := b.validatePath(path); err != nil {
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
		return nil, fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxFileSize)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %w", err)
	}

	// check if binary
	if isBinary(content) {
		return nil, fmt.Errorf("unsupported file type: binary")
	}

	lang := langExts[ext]

	return &FileView{
		Path:     path,
		Type:     "text",
		Content:  string(content),
		Language: lang,
		Size:     info.Size(),
	}, nil
}

func (b *Browser) ServeRaw(w http.ResponseWriter, r *http.Request, path string) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if err := b.validatePath(absPath); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.ServeFile(w, r, absPath)
}

func (b *Browser) validatePath(path string) error {
	// resolve symlinks to prevent symlink traversal
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// if file doesn't exist yet, resolve the parent
		resolved, err = filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("access denied: cannot resolve path")
		}
		resolved = filepath.Join(resolved, filepath.Base(path))
	}

	home, _ := os.UserHomeDir()
	homeResolved, _ := filepath.EvalSymlinks(home)

	// use path separator suffix to prevent /Users/loppo-evil matching /Users/loppo
	allowedRoots := []string{
		homeResolved + string(filepath.Separator),
		"/tmp" + string(filepath.Separator),
	}

	// exact match on root itself
	if resolved == homeResolved {
		return nil
	}

	for _, root := range allowedRoots {
		if strings.HasPrefix(resolved+string(filepath.Separator), root) {
			return nil
		}
	}

	return fmt.Errorf("access denied: path must be under home directory")
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
