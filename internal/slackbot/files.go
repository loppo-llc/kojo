package slackbot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// uploadDir matches the WebUI upload directory (file_handlers.go).
var uploadDir = filepath.Join(os.TempDir(), "kojo", "upload")

// maxFileSize is the maximum file size (20 MB) the bot will download.
const maxFileSize = 20 * 1024 * 1024

// fileDownloadTimeout caps how long a single Slack attachment download may
// take. Using http.DefaultClient directly would allow a hung upstream to
// block the handler goroutine indefinitely.
const fileDownloadTimeout = 60 * time.Second

// slackFileHTTPClient is the HTTP client used for Slack file downloads.
// Exposed as a package variable so tests can swap it (e.g. to route through
// httptest.Server with a short timeout).
var slackFileHTTPClient = &http.Client{Timeout: fileDownloadTimeout}

// preflightSlackFile validates a Slack file descriptor before we spend any
// I/O on it. Returns a nil error when the file is eligible for download.
func preflightSlackFile(f slack.File) error {
	if f.Size > maxFileSize {
		return fmt.Errorf("file too large (%d bytes, max %d)", f.Size, maxFileSize)
	}
	if f.URLPrivateDownload == "" {
		return fmt.Errorf("no download URL available")
	}
	return nil
}

// buildLocalPath returns the destination path for a Slack attachment using
// the WebUI upload convention {unixnano}_{sanitized_original_name}. Pure.
func buildLocalPath(dir string, now time.Time, originalName string) string {
	return filepath.Join(dir, fmt.Sprintf("%d_%s", now.UnixNano(), sanitizeFilename(originalName)))
}

// downloadedFile holds metadata about a successfully downloaded Slack file.
type downloadedFile struct {
	Path string // local filesystem path
	Name string // original filename
	Mime string // MIME type
	Size int    // file size in bytes
}

// fileError holds metadata about a file that failed to download.
type fileError struct {
	Name string // original filename
	Err  string // human-readable error
}

// downloadSlackFiles downloads Slack file attachments to the shared upload
// directory ({os.TempDir()}/kojo/upload, same as the WebUI) and returns
// metadata for each successfully downloaded file. Files that are too large
// or fail to download are recorded in errors so the caller can inform the
// agent.
func (b *Bot) downloadSlackFiles(ctx context.Context, files []slack.File) ([]downloadedFile, []fileError) {
	if len(files) == 0 {
		return nil, nil
	}

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		b.logger.Warn("failed to create upload dir", "err", err)
		var errs []fileError
		for _, f := range files {
			errs = append(errs, fileError{Name: f.Name, Err: fmt.Sprintf("upload dir not writable: %v", err)})
		}
		return nil, errs
	}

	var result []downloadedFile
	var errs []fileError
	for _, f := range files {
		if err := preflightSlackFile(f); err != nil {
			b.logger.Warn("slack file skipped", "fileID", f.ID, "name", f.Name, "err", err)
			errs = append(errs, fileError{Name: f.Name, Err: err.Error()})
			continue
		}

		localPath, err := b.downloadOneFile(ctx, uploadDir, &f)
		if err != nil {
			b.logger.Warn("failed to download slack file",
				"fileID", f.ID, "name", f.Name, "err", err)
			errs = append(errs, fileError{Name: f.Name, Err: err.Error()})
			continue
		}

		result = append(result, downloadedFile{
			Path: localPath,
			Name: f.Name,
			Mime: f.Mimetype,
			Size: f.Size,
		})
		b.logger.Info("downloaded slack file",
			"fileID", f.ID, "name", f.Name, "path", localPath, "size", f.Size)
	}
	return result, errs
}

// downloadOneFile downloads a single Slack file and saves it locally.
// The filename uses {unixnano}_{original_name} to match the WebUI upload convention.
func (b *Bot) downloadOneFile(ctx context.Context, dir string, f *slack.File) (string, error) {
	localPath := buildLocalPath(dir, time.Now(), f.Name)

	// Slack private URLs require bot token in Authorization header.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URLPrivateDownload, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.botToken)

	// Use the package-level client so downloads can't hang forever on a
	// stuck upstream (http.DefaultClient has no timeout).
	resp, err := slackFileHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// Use an explicit restrictive permission (0600) so downloaded attachments
	// are not readable by other local users in the shared temp directory.
	out, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}

	// Cap the download size. io.LimitReader silently truncates, so we read
	// up to maxFileSize+1 and reject the result if it exceeds the limit.
	n, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxFileSize+1))
	// Close before any removal attempt — on Windows an open handle blocks
	// os.Remove, leaving a partial file behind.
	closeErr := out.Close()
	switch {
	case copyErr != nil:
		if rmErr := os.Remove(localPath); rmErr != nil {
			b.logger.Warn("failed to remove partial download", "path", localPath, "err", rmErr)
		}
		return "", fmt.Errorf("write file: %w", copyErr)
	case n > maxFileSize:
		if rmErr := os.Remove(localPath); rmErr != nil {
			b.logger.Warn("failed to remove oversize download", "path", localPath, "err", rmErr)
		}
		return "", fmt.Errorf("file exceeds max size (%d bytes, max %d)", n, maxFileSize)
	case closeErr != nil:
		if rmErr := os.Remove(localPath); rmErr != nil {
			b.logger.Warn("failed to remove after close error", "path", localPath, "err", rmErr)
		}
		return "", fmt.Errorf("close file: %w", closeErr)
	}

	return localPath, nil
}

// appendFileInfo appends downloaded file paths and any errors to the message
// text so the agent knows about the files and can read them with its file tools.
func appendFileInfo(text string, files []downloadedFile, errs []fileError) string {
	if len(files) == 0 && len(errs) == 0 {
		return text
	}
	var sb strings.Builder
	sb.WriteString(text)
	if text != "" {
		sb.WriteString("\n\n")
	}
	if len(files) > 0 {
		sb.WriteString("[Attached files]\n")
		for _, f := range files {
			sb.WriteString(fmt.Sprintf("- %s (%s, %d bytes): %s\n", f.Name, f.Mime, f.Size, f.Path))
		}
	}
	if len(errs) > 0 {
		sb.WriteString("[File download errors]\n")
		for _, e := range errs {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", e.Name, e.Err))
		}
	}
	return sb.String()
}

// sanitizeFilename removes path separators and other problematic characters.
func sanitizeFilename(name string) string {
	name = filepath.Base(name) // strip any directory components
	// Replace problematic characters with underscore.
	replacer := strings.NewReplacer("/", "_", "\\", "_", "\x00", "_")
	return replacer.Replace(name)
}
