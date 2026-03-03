package session

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Attachment represents a media file detected in session output.
type Attachment struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Mime      string `json:"mime"`
	ModTime   string `json:"modTime"`
	CreatedAt string `json:"createdAt"`
}

var mediaExts = map[string]string{
	// images
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".bmp":  "image/bmp",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
	".tiff": "image/tiff",
	".tif":  "image/tiff",
	".avif": "image/avif",
	// videos
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".avi":  "video/x-msvideo",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".flv":  "video/x-flv",
	".wmv":  "video/x-ms-wmv",
	".m4v":  "video/x-m4v",
}

const attachTailSize = 8192

// Build a dynamic extension alternation from mediaExts keys.
// Sort longest-first so `.tiff` matches before `.tif`.
func init() {
	exts := make([]string, 0, len(mediaExts))
	for ext := range mediaExts {
		exts = append(exts, ext)
	}
	sort.Slice(exts, func(i, j int) bool {
		if len(exts[i]) != len(exts[j]) {
			return len(exts[i]) > len(exts[j])
		}
		return exts[i] < exts[j]
	})
	quoted := make([]string, len(exts))
	for i, ext := range exts {
		quoted[i] = regexp.QuoteMeta(ext)
	}
	extPattern := strings.Join(quoted, "|")

	// Unquoted path: /abs, ./rel, ~/home, C:\ — no whitespace allowed in path (case-insensitive)
	mediaPathUnquotedRe = regexp.MustCompile(`(?i)(?:(?:/|\.{1,2}[/\\]|~/|[A-Za-z]:[/\\])[^\s"'` + "`" + `<>|;(){}]+(?:` + extPattern + `))`)
	// Quoted path: "path with spaces.png" or 'path with spaces.png' (case-insensitive)
	mediaPathQuotedRe = regexp.MustCompile(`(?i)["']([^"']+(?:` + extPattern + `))["']`)
}

var (
	mediaPathUnquotedRe *regexp.Regexp
	mediaPathQuotedRe   *regexp.Regexp
)

// CheckAttachments appends data to the trailing buffer, scans for media file paths,
// and returns newly detected attachments. Caller should broadcast the result.
func (s *Session) CheckAttachments(data []byte) []*Attachment {
	s.mu.Lock()
	s.attachTail = append(s.attachTail, data...)
	if len(s.attachTail) > attachTailSize {
		s.attachTail = s.attachTail[len(s.attachTail)-attachTailSize:]
	}
	tail := make([]byte, len(s.attachTail))
	copy(tail, s.attachTail)
	workDir := s.WorkDir
	// snapshot known paths to skip duplicates without holding lock during I/O
	known := make(map[string]struct{}, len(s.attachments))
	for k := range s.attachments {
		known[k] = struct{}{}
	}
	s.mu.Unlock()

	// strip ANSI
	clean := AnsiRe.ReplaceAll(tail, []byte(" "))

	// collect candidate paths
	candidates := make(map[string]struct{})

	for _, m := range mediaPathUnquotedRe.FindAll(clean, -1) {
		candidates[string(m)] = struct{}{}
	}
	for _, m := range mediaPathQuotedRe.FindAllSubmatch(clean, -1) {
		candidates[string(m[1])] = struct{}{}
	}

	home, _ := os.UserHomeDir()

	// resolve and stat outside the lock
	type candidate struct {
		resolved string
		info     os.FileInfo
		mime     string
	}
	var verified []candidate

	for path := range candidates {
		resolved := path
		if strings.HasPrefix(resolved, "~/") && home != "" {
			resolved = filepath.Join(home, resolved[2:])
		} else if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(workDir, resolved)
		}
		resolved, _ = filepath.Abs(filepath.Clean(resolved))

		if _, exists := known[resolved]; exists {
			continue
		}

		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			continue
		}

		ext := strings.ToLower(filepath.Ext(resolved))
		mime, ok := mediaExts[ext]
		if !ok {
			continue
		}

		verified = append(verified, candidate{resolved: resolved, info: info, mime: mime})
	}

	if len(verified) == 0 {
		return nil
	}

	// lock only for map update
	s.mu.Lock()
	defer s.mu.Unlock()

	var newAttachments []*Attachment
	for _, c := range verified {
		if _, exists := s.attachments[c.resolved]; exists {
			continue // another goroutine added it
		}
		att := &Attachment{
			Path:      c.resolved,
			Name:      filepath.Base(c.resolved),
			Size:      c.info.Size(),
			Mime:      c.mime,
			ModTime:   c.info.ModTime().UTC().Format(time.RFC3339),
			CreatedAt: fileCreationTime(c.info).UTC().Format(time.RFC3339),
		}
		s.attachments[c.resolved] = att
		newAttachments = append(newAttachments, att)
	}

	return newAttachments
}

// Attachments returns all tracked attachments, removing any whose files no longer exist.
func (s *Session) Attachments() []*Attachment {
	// snapshot paths under lock
	s.mu.Lock()
	paths := make([]string, 0, len(s.attachments))
	atts := make(map[string]*Attachment, len(s.attachments))
	for path, att := range s.attachments {
		paths = append(paths, path)
		atts[path] = att
	}
	s.mu.Unlock()

	// stat outside lock
	var result []*Attachment
	var gone []string
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			gone = append(gone, path)
			continue
		}
		result = append(result, atts[path])
	}

	// remove gone entries
	if len(gone) > 0 {
		s.mu.Lock()
		for _, path := range gone {
			delete(s.attachments, path)
		}
		s.mu.Unlock()
	}

	return result
}

// HasAttachment checks if a path is tracked as an attachment.
func (s *Session) HasAttachment(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.attachments[path]
	return exists
}

// RemoveAttachment removes an attachment from tracking (does not delete the file).
func (s *Session) RemoveAttachment(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attachments, path)
}
