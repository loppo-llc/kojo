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

// downloadSlackFiles downloads Slack file attachments to the agent's data
// directory and returns metadata for each successfully downloaded file.
// Files that are too large or fail to download are recorded in errors
// so the caller can inform the agent.
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
		if f.Size > maxFileSize {
			b.logger.Warn("slack file too large, skipping",
				"fileID", f.ID, "name", f.Name, "size", f.Size)
			errs = append(errs, fileError{Name: f.Name, Err: fmt.Sprintf("file too large (%d bytes, max %d)", f.Size, maxFileSize)})
			continue
		}
		if f.URLPrivateDownload == "" {
			b.logger.Debug("slack file has no download URL, skipping",
				"fileID", f.ID, "name", f.Name)
			errs = append(errs, fileError{Name: f.Name, Err: "no download URL available"})
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
	safeName := sanitizeFilename(f.Name)
	localName := fmt.Sprintf("%d_%s", time.Now().UnixNano(), safeName)
	localPath := filepath.Join(dir, localName)

	// Slack private URLs require bot token in Authorization header.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URLPrivateDownload, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.botToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	out, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, io.LimitReader(resp.Body, maxFileSize+1)); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("write file: %w", err)
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
