package slackbot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/slack-go/slack"
)

// maxFileSize is the maximum file size (20 MB) the bot will download.
const maxFileSize = 20 * 1024 * 1024

// downloadedFile holds metadata about a successfully downloaded Slack file.
type downloadedFile struct {
	Path string // local filesystem path
	Name string // original filename
	Mime string // MIME type
	Size int    // file size in bytes
}

// downloadSlackFiles downloads Slack file attachments to the agent's data
// directory and returns metadata for each successfully downloaded file.
// Files that are too large or fail to download are logged and skipped.
func (b *Bot) downloadSlackFiles(ctx context.Context, files []slack.File) []downloadedFile {
	if b.agentDataDir == "" || len(files) == 0 {
		return nil
	}

	dir := filepath.Join(b.agentDataDir, "slack-files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.logger.Warn("failed to create slack-files dir", "err", err)
		return nil
	}

	var result []downloadedFile
	for _, f := range files {
		if f.Size > maxFileSize {
			b.logger.Warn("slack file too large, skipping",
				"fileID", f.ID, "name", f.Name, "size", f.Size)
			continue
		}
		if f.URLPrivateDownload == "" {
			b.logger.Debug("slack file has no download URL, skipping",
				"fileID", f.ID, "name", f.Name)
			continue
		}

		localPath, err := b.downloadOneFile(ctx, dir, &f)
		if err != nil {
			b.logger.Warn("failed to download slack file",
				"fileID", f.ID, "name", f.Name, "err", err)
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
	return result
}

// downloadOneFile downloads a single Slack file and saves it locally.
// The filename includes the Slack file ID prefix to avoid collisions.
func (b *Bot) downloadOneFile(ctx context.Context, dir string, f *slack.File) (string, error) {
	// Build safe filename: "{fileID}_{original_name}"
	safeName := sanitizeFilename(f.Name)
	localName := fmt.Sprintf("%s_%s", f.ID, safeName)
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

// appendFilePaths appends downloaded file paths to the message text so the
// agent knows about the files and can read them with its file tools.
func appendFilePaths(text string, files []downloadedFile) string {
	var sb strings.Builder
	sb.WriteString(text)
	if text != "" {
		sb.WriteString("\n\n")
	}
	sb.WriteString("[Attached files]\n")
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("- %s (%s, %d bytes): %s\n", f.Name, f.Mime, f.Size, f.Path))
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
