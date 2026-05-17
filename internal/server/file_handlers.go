package server

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/filebrowser"
)

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

func (s *Server) handleViewFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	result, err := s.files.View(path)
	if err != nil {
		if errors.Is(err, filebrowser.ErrUnsupportedFile) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
		} else if errors.Is(err, filebrowser.ErrFileTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleRawFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	}
	s.files.ServeRaw(w, r, path)
}

// --- Upload Handler ---

var uploadDir = filepath.Join(os.TempDir(), "kojo", "upload")

// maxUploadSize caps how large a single attachment upload may be. Set
// to 10 GiB so that legitimate large transfers (videos, datasets,
// model files, etc.) succeed; this is a local/Tailscale-only tool so
// the usual public-endpoint DoS concerns don't apply.
const maxUploadSize = 10 << 30 // 10 GiB

// maxUploadInMemory is the in-memory threshold passed to
// ParseMultipartForm; anything above this spills to a temp file. Keep
// this small so we don't accidentally hold a multi-GB body in RAM
// when the cap above grows.
const maxUploadInMemory = 32 << 20 // 32 MiB

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadInMemory); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file too large (max 10GiB)")
		return
	}
	// ParseMultipartForm spills bodies above maxUploadInMemory to
	// os.TempDir. Without RemoveAll those temp files survive until
	// the OS cleans the temp dir, which on a 10 GiB cap is a real
	// disk-leak vector. Defer the cleanup so it runs whether the
	// handler succeeds or aborts mid-flight.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing file field")
		return
	}
	defer file.Close()

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create upload directory")
		return
	}

	safeName := sanitizeFilename(filepath.Base(header.Filename))
	filename := fmt.Sprintf("%d_%s", time.Now().UnixNano(), safeName)
	destPath := filepath.Join(uploadDir, filename)

	dst, err := os.Create(destPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create file")
		return
	}
	defer dst.Close()

	written, err := dst.ReadFrom(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to write file")
		return
	}

	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{
		"path": destPath,
		"name": header.Filename,
		"size": written,
		"mime": mime,
	})
}

func cleanupUploads() {
	os.RemoveAll(uploadDir)
}
