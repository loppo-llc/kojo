// Package webdav implements a kojo-flavored WebDAV server. It wraps
// `golang.org/x/net/webdav.Handler` over a constrained file-system view
// rooted at <configdir>/webdav/, and adds the kojo-specific safety
// rails the design doc (§4.3 / §5.6) calls out:
//
//   - NFC normalization on incoming paths (refuse non-NFC up front
//     rather than silently rewriting; matches blob.validatePath).
//   - Reserved-filename auto-discard (Thumbs.db, .DS_Store, AppleDouble
//     `._*` shadows, Office lock `~$*` shadows). PUT/DELETE on these
//     short-circuit to 200 OK without touching disk so a Mac/Windows
//     client scanning the share doesn't get noisy 4xx errors but also
//     doesn't pollute the share with OS junk.
//   - Case-collision 409 on case-sensitive hosts (macOS APFS / Linux
//     ext4) so `Foo.txt` PUT against an existing `foo.txt` doesn't
//     silently coexist as two distinct paths that a case-folding peer
//     (Windows / older macOS HFS+) would later collide.
//
// The mount is intentionally narrow: a single shared area for ad-hoc
// files (drag-and-drop attachments the user wants the agent to see).
// Per-agent scoping or larger blob-store integration are future work.
package webdav

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/webdav"
	"golang.org/x/text/unicode/norm"
)

// Reserved-filename / prefix lists. Kept in sync with
// internal/blob/path.go's lists by code review (the lists are
// short + stable; a runtime cross-import would couple blob's
// internal validators into the WebDAV public surface).
var reservedNames = map[string]bool{
	".ds_store":   true,
	"thumbs.db":   true,
	"desktop.ini": true,
}

var reservedPrefixes = []string{
	"._",     // macOS AppleDouble
	"~$",     // Office lock files
	".tmp-",  // atomicfile / blob temp prefix
	".blob-", // blob package's atomicWrite temp
}

// MountConfig wires a WebDAV handler over the given root directory.
type MountConfig struct {
	// Root is the directory the WebDAV view is rooted at. Must
	// be MkdirAll-creatable. Paths outside Root are unreachable
	// through the handler (validated via filepath.Rel below).
	Root string
	// Prefix is the URL path prefix to strip before resolving
	// against Root. Typically "/api/v1/webdav".
	Prefix string
	// Logger receives operational warnings (collision reports,
	// reserved-filename auto-discards). nil → slog.Default.
	Logger *slog.Logger
}

// Handler returns an http.Handler that serves WebDAV. The caller
// is responsible for wrapping it in auth middleware (owner-only
// gate + short-lived bearer-token validation).
func Handler(cfg MountConfig) (http.Handler, error) {
	if cfg.Root == "" {
		return nil, errors.New("webdav: Root required")
	}
	if cfg.Prefix == "" {
		return nil, errors.New("webdav: Prefix required")
	}
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return nil, fmt.Errorf("webdav: ensure root: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	rootAbs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("webdav: abs root: %w", err)
	}
	fs := &constrainedFS{
		root:   rootAbs,
		logger: logger,
	}
	h := &webdav.Handler{
		Prefix:     cfg.Prefix,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				logger.Warn("webdav request error",
					"method", r.Method, "url", r.URL.Path, "err", err)
			}
		},
	}
	return h, nil
}

// constrainedFS implements webdav.FileSystem with the kojo safety
// rails. Filesystem-backed (paths resolve under .root); reserved-
// name short-circuiting is in the per-method handlers below.
type constrainedFS struct {
	root   string
	logger *slog.Logger
}

// resolve normalizes and validates a webdav-supplied path,
// returning the absolute on-disk path. The named error sentinels
// inform the caller's response shape:
//
//   - errReservedSkip → reserved filename; PUT/MKCOL short-circuit
//     to 200 OK without touching disk (caller-specific). Read paths
//     map this to ErrNotExist so the file listing simply omits the
//     name.
//   - fs.ErrInvalid   → non-NFC, NUL byte, or other malformed input.
//     webdav.Handler maps this to 403.
//   - fs.ErrPermission → traversal attempt or out-of-root resolve.
//     webdav.Handler maps to 403.
func (c *constrainedFS) resolve(name string) (string, error) {
	if !norm.NFC.IsNormalString(name) {
		return "", fs.ErrInvalid
	}
	// Reject `..` / `.` segments BEFORE path.Clean collapses
	// them. A WebDAV `Destination: /api/v1/webdav/../x` header
	// would otherwise normalize to root/x without us noticing
	// the traversal attempt. The clean-then-Rel guard below is
	// belt-and-suspenders for the same case.
	for _, seg := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		if seg == ".." {
			return "", fs.ErrPermission
		}
	}
	cleaned := path.Clean("/" + name)
	if cleaned == "/" {
		return c.root, nil
	}
	rel := strings.TrimPrefix(cleaned, "/")
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		if err := validateSegment(seg); err != nil {
			return "", err
		}
	}
	full := filepath.Join(c.root, filepath.FromSlash(rel))
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	relCheck, err := filepath.Rel(c.root, fullAbs)
	if err != nil || strings.HasPrefix(relCheck, "..") || filepath.IsAbs(relCheck) {
		return "", fs.ErrPermission
	}
	return fullAbs, nil
}

// validateSegment rejects reserved names + dangerous characters.
// Reserved-name matches return errReservedSkip so callers can
// short-circuit without an error response.
func validateSegment(seg string) error {
	if seg == "." || seg == ".." {
		return fs.ErrPermission
	}
	if strings.ContainsRune(seg, 0) {
		return fs.ErrInvalid
	}
	// Reject backslash inside a segment. WebDAV path semantics
	// use forward-slash exclusively, but a Windows client might
	// send `subdir\foo.txt` as a literal segment; on Windows
	// hosts the OS would interpret the backslash as a separator
	// and bypass our reserved-name + traversal checks.
	if strings.ContainsRune(seg, '\\') {
		return fs.ErrInvalid
	}
	lower := strings.ToLower(seg)
	if reservedNames[lower] {
		return errReservedSkip
	}
	for _, pref := range reservedPrefixes {
		if strings.HasPrefix(lower, pref) {
			return errReservedSkip
		}
	}
	return nil
}

// errReservedSkip is the sentinel for reserved-filename auto-
// discard. Per-method handlers translate it to a 200 OK no-op
// (write paths) or fs.ErrNotExist (read paths).
var errReservedSkip = errors.New("webdav: reserved filename")

// IsReservedFilename reports whether err originated from
// validateSegment's reserved-name check. Useful for callers that
// want to log auto-discards.
func IsReservedFilename(err error) bool { return errors.Is(err, errReservedSkip) }

// caseCollision reports whether name would collide with an
// existing entry under dir when compared case-insensitively.
// Returns the existing path (or "") so the caller can include it
// in the 409 error.
func caseCollision(dir, name string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	lower := strings.ToLower(name)
	for _, e := range entries {
		if e.Name() == name {
			// Exact match isn't a collision — that's a normal overwrite.
			return "", false
		}
		if strings.ToLower(e.Name()) == lower {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

func (c *constrainedFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	full, err := c.resolve(name)
	if err != nil {
		if errors.Is(err, errReservedSkip) {
			c.logger.Debug("webdav: Mkdir on reserved name; auto-discarding", "name", name)
			return nil
		}
		return err
	}
	if existing, hit := caseCollision(filepath.Dir(full), filepath.Base(full)); hit {
		c.logger.Warn("webdav: case collision on Mkdir",
			"want", name, "exists", existing)
		// webdav.Handler maps fs.ErrExist to 409 Conflict, which
		// is what the design doc wants for case-collision.
		return fs.ErrExist
	}
	return os.Mkdir(full, perm)
}

func (c *constrainedFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	full, err := c.resolve(name)
	if err != nil {
		if errors.Is(err, errReservedSkip) {
			// On read paths, a reserved name is "not there"; on
			// write paths it's an auto-discard (open a tmp sink
			// the caller's writes go to and are discarded on close).
			if flag&os.O_CREATE != 0 || flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0 {
				c.logger.Debug("webdav: write to reserved name; auto-discarding",
					"name", name)
				return &discardFile{name: name}, nil
			}
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	// Case-collision check on create paths only — a read of an
	// existing file with exact match falls through to the OS,
	// which on case-folding hosts (HFS+) would case-fold the name
	// and read whatever we have. We don't fight that.
	if flag&os.O_CREATE != 0 {
		if existing, hit := caseCollision(filepath.Dir(full), filepath.Base(full)); hit {
			c.logger.Warn("webdav: case collision on OpenFile create",
				"want", name, "exists", existing)
			return nil, fs.ErrExist
		}
	}
	f, err := os.OpenFile(full, flag, perm)
	if err != nil {
		return nil, err
	}
	return &filteredFile{File: f}, nil
}

// filteredFile wraps *os.File so Readdir omits reserved-name
// entries that snuck onto the share via direct filesystem access
// (a user dropped a .DS_Store from a Mac, etc.). Without this
// filter, PROPFIND would surface the entry, then a follow-up Stat
// would 404 (resolve() rejects reserved names), and the WebDAV
// xml stream would abort.
type filteredFile struct {
	*os.File
}

func (f *filteredFile) Readdir(count int) ([]os.FileInfo, error) {
	all, err := f.File.Readdir(count)
	if err != nil {
		return all, err
	}
	out := all[:0]
	for _, fi := range all {
		if shouldHideFromListing(fi.Name()) {
			continue
		}
		out = append(out, fi)
	}
	return out, nil
}

// shouldHideFromListing returns true for any directory entry that
// the WebDAV layer would otherwise refuse to Stat. Without this,
// PROPFIND would surface the entry, the follow-up Stat in the
// xml-stream walker would 404 / 403, and the entire stream would
// abort. Hiding the entry from the listing keeps PROPFIND working
// for the legitimate siblings.
//
// Drops:
//   - reserved names (.DS_Store etc., reserved prefixes)
//   - non-NFC names (resolve() rejects)
//   - segments validateSegment would reject (NUL, backslash, `..`)
func shouldHideFromListing(name string) bool {
	if isReservedName(name) {
		return true
	}
	if !norm.NFC.IsNormalString(name) {
		return true
	}
	if err := validateSegment(name); err != nil && !errors.Is(err, errReservedSkip) {
		return true
	}
	return false
}

// isReservedName mirrors validateSegment's reserved-name check
// for the read-side filter. Returning true means "hide from
// directory listings" — the corresponding write-side path
// auto-discards.
func isReservedName(name string) bool {
	lower := strings.ToLower(name)
	if reservedNames[lower] {
		return true
	}
	for _, pref := range reservedPrefixes {
		if strings.HasPrefix(lower, pref) {
			return true
		}
	}
	return false
}

func (c *constrainedFS) RemoveAll(ctx context.Context, name string) error {
	full, err := c.resolve(name)
	if err != nil {
		if errors.Is(err, errReservedSkip) {
			c.logger.Debug("webdav: RemoveAll on reserved name; auto-discarding",
				"name", name)
			return nil
		}
		return err
	}
	if full == c.root {
		// Refuse to delete the root itself — that would unmount
		// the server.
		return fs.ErrPermission
	}
	return os.RemoveAll(full)
}

func (c *constrainedFS) Rename(ctx context.Context, oldName, newName string) error {
	oldFull, err := c.resolve(oldName)
	if err != nil {
		if errors.Is(err, errReservedSkip) {
			return fs.ErrNotExist
		}
		return err
	}
	newFull, err := c.resolve(newName)
	if err != nil {
		if errors.Is(err, errReservedSkip) {
			c.logger.Debug("webdav: Rename to reserved name; auto-discarding source",
				"old", oldName, "new", newName)
			// Discard by removing the source — the rename "succeeded"
			// in the OS-junk sense (the pseudo-target doesn't exist).
			return os.RemoveAll(oldFull)
		}
		return err
	}
	if existing, hit := caseCollision(filepath.Dir(newFull), filepath.Base(newFull)); hit && existing != newFull {
		c.logger.Warn("webdav: case collision on Rename",
			"new", newName, "exists", existing)
		return fs.ErrExist
	}
	return os.Rename(oldFull, newFull)
}

func (c *constrainedFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	full, err := c.resolve(name)
	if err != nil {
		if errors.Is(err, errReservedSkip) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return os.Stat(full)
}

// discardFile is the write-only sink for reserved-filename auto-
// discards. Implements webdav.File so the handler's PUT pipeline
// can copy bytes into it without the caller ever knowing the data
// went to /dev/null.
type discardFile struct {
	name string
}

func (d *discardFile) Read(p []byte) (int, error)              { return 0, fs.ErrInvalid }
func (d *discardFile) Write(p []byte) (int, error)             { return len(p), nil }
func (d *discardFile) Seek(offset int64, whence int) (int64, error) { return 0, nil }
func (d *discardFile) Close() error                            { return nil }
func (d *discardFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, fs.ErrInvalid
}
func (d *discardFile) Stat() (os.FileInfo, error) {
	return discardFileInfo{name: d.name}, nil
}

// discardFileInfo is a minimal FileInfo for the discard sink.
type discardFileInfo struct{ name string }

func (d discardFileInfo) Name() string       { return d.name }
func (d discardFileInfo) Size() int64        { return 0 }
func (d discardFileInfo) Mode() os.FileMode  { return 0 }
func (d discardFileInfo) ModTime() time.Time { return time.Time{} }
func (d discardFileInfo) IsDir() bool        { return false }
func (d discardFileInfo) Sys() any           { return nil }
