package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/loppo-llc/kojo/internal/filebrowser"
	"github.com/loppo-llc/kojo/internal/thumbnail"
	"github.com/loppo-llc/kojo/internal/uploadpath"
)

// codeForStatus maps an HTTP status onto the canonical error code used in the
// JSON envelope for the raw/thumb file-serving endpoints.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	default:
		return "internal_error"
	}
}

// writeServeErr delivers a filebrowser/thumbnail serving error (returned as
// *thumbnail.HTTPError before any bytes were streamed) as the server's JSON
// error envelope. A non-HTTPError is treated as an internal error.
func writeServeErr(w http.ResponseWriter, err error) {
	var he *thumbnail.HTTPError
	if errors.As(err, &he) {
		writeError(w, he.Status, codeForStatus(he.Status), he.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
}

// --- File Browser Handlers ---

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	hidden := r.URL.Query().Get("hidden") == "true"

	result, err := s.files.List(dir, hidden)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

// writeFileViewError maps a filebrowser.View error onto the status
// ladder shared by the global and agent-scoped file-view endpoints:
// unsupported file type → 415, size cap → 413, anything else → 400.
// The body carries err.Error() verbatim in every branch.
func writeFileViewError(w http.ResponseWriter, err error) {
	if errors.Is(err, filebrowser.ErrUnsupportedFile) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
	} else if errors.Is(err, filebrowser.ErrFileTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
	} else {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

func (s *Server) handleViewFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	result, err := s.files.View(path)
	if err != nil {
		writeFileViewError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

// fileWriteBodyCap bounds the JSON body of a text-file save. The file
// itself is capped at 1 MiB by filebrowser; JSON escaping can inflate
// that up to ~6x (\uXXXX for control characters), so allow headroom.
const fileWriteBodyCap = 8 << 20

type fileWriteRequest struct {
	Content string `json:"content"`
}

// readFileWrite parses the shared preamble of the global and agent-scoped
// text-save endpoints: a mandatory, non-wildcard If-Match carrying the
// etag the viewer loaded, and the JSON body. On failure it has already
// written the response.
func (s *Server) readFileWrite(w http.ResponseWriter, r *http.Request) (ifMatch, content string, ok bool) {
	ifMatch, present, ok := s.parseIfMatchStrict(w, r, "If-Match wildcard is not supported for file saves")
	if !ok {
		return "", "", false
	}
	if !present {
		writeError(w, http.StatusPreconditionRequired, "precondition_required",
			"If-Match with the etag from the view response is required")
		return "", "", false
	}
	var req fileWriteRequest
	if !readCappedJSON(w, r, fileWriteBodyCap, "request body too large", "invalid JSON body", &req) {
		return "", "", false
	}
	return ifMatch, req.Content, true
}

// writeFileSaveError maps a filebrowser.WriteText error: etag mismatch →
// 412, everything else follows the view endpoint's ladder.
func writeFileSaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, filebrowser.ErrETagMismatch) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed", err.Error())
		return
	}
	writeFileViewError(w, err)
}

// handleWriteFile saves an edited text file (the file viewer's editor).
// Owner (and trusted peer, for the remote session file tab) only — see
// auth policy.
func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	ifMatch, content, ok := s.readFileWrite(w, r)
	if !ok {
		return
	}
	view, err := s.files.WriteText(path, content, ifMatch)
	if err != nil {
		writeFileSaveError(w, err)
		return
	}
	w.Header().Set("ETag", quoteETag(view.ETag))
	writeJSONResponse(w, http.StatusOK, view)
}

func (s *Server) handleRawFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	}
	if err := s.files.ServeRaw(w, r, path); err != nil {
		writeServeErr(w, err)
	}
}

// handleThumbFile serves a JPEG thumbnail for an arbitrary user-space
// image. Used by the attachments grid / inline message previews so a
// 5-MB screenshot doesn't have to ship in full just to render a 150-px
// tile.
func (s *Server) handleThumbFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if err := s.files.ServeThumb(w, r, path, size); err != nil {
		writeServeErr(w, err)
	}
}

// --- Upload Handler ---

var uploadDir = uploadpath.Dir()

// maxUploadSize caps how large a single attachment upload may be. Set
// to 10 GiB so that legitimate large transfers (videos, datasets,
// model files, etc.) succeed; this is a local/Tailscale-only tool so
// the usual public-endpoint DoS concerns don't apply.
const maxUploadSize = 10 << 30 // 10 GiB

// Multipart framing is not file content. Leave a small bounded allowance so
// a file at the advertised ceiling is not rejected solely because of its
// Content-Disposition and boundary bytes.
const maxUploadRequestSize = maxUploadSize + (1 << 20)

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestSize)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing file field")
		return
	}
	var fileName, contentType string
	var part io.ReadCloser
	for {
		p, nextErr := mr.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(nextErr, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file too large (max 10GiB)")
				return
			}
			writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart upload")
			return
		}
		if p.FormName() == "file" && p.FileName() != "" {
			part = p
			fileName = p.FileName()
			contentType = p.Header.Get("Content-Type")
			break
		}
		_ = p.Close()
	}
	if part == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing file field")
		return
	}
	defer part.Close()

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create upload directory")
		return
	}

	safeName := uploadpath.SanitizeName(fileName)
	filename := fmt.Sprintf("%d_%s", time.Now().UnixNano(), safeName)
	destPath := filepath.Join(uploadDir, filename)

	// Stream into a restrictive temporary file and only publish the final
	// path after both the copy and Close succeed. This avoids the former
	// ParseMultipartForm double-spool for large peer transfers and prevents
	// partially-written files from being accepted by a concurrent chat.
	dst, err := os.CreateTemp(uploadDir, ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create file")
		return
	}
	tmpPath := dst.Name()
	keep := false
	defer func() {
		_ = dst.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := dst.Chmod(0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to secure file")
		return
	}

	written, copyErr := io.Copy(dst, io.LimitReader(part, maxUploadSize+1))
	if copyErr != nil {
		var maxErr *http.MaxBytesError
		if errors.As(copyErr, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file too large (max 10GiB)")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to write file")
		return
	}
	if written > maxUploadSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file too large (max 10GiB)")
		return
	}
	if err := dst.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to finalize file")
		return
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish file")
		return
	}
	keep = true

	mime := contentType
	if mime == "" {
		mime = "application/octet-stream"
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{
		"path": destPath,
		"name": fileName,
		"size": written,
		"mime": mime,
		"peerId": func() string {
			if s.peerID != nil {
				return s.peerID.DeviceID
			}
			return ""
		}(),
	})
}

func cleanupUploads() {
	os.RemoveAll(uploadDir)
}
