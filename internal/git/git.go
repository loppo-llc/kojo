package git

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

type Manager struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) *Manager {
	return &Manager{logger: logger}
}

type StatusResult struct {
	Branch    string   `json:"branch"`
	Ahead     int      `json:"ahead"`
	Behind    int      `json:"behind"`
	Staged    []string `json:"staged"`
	Modified  []string `json:"modified"`
	Untracked []string `json:"untracked"`
}

func (m *Manager) Status(workDir string) (*StatusResult, error) {
	if workDir == "" {
		return nil, fmt.Errorf("workDir is required")
	}

	result := &StatusResult{
		Staged:    []string{},
		Modified:  []string{},
		Untracked: []string{},
	}

	// branch name
	branch, err := m.run(workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	result.Branch = strings.TrimSpace(branch)

	// ahead/behind
	ab, _ := m.run(workDir, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if ab != "" {
		parts := strings.Fields(strings.TrimSpace(ab))
		if len(parts) == 2 {
			result.Ahead, _ = strconv.Atoi(parts[0])
			result.Behind, _ = strconv.Atoi(parts[1])
		}
	}

	// porcelain status
	status, err := m.run(workDir, "status", "--porcelain=v1")
	if err != nil {
		return nil, err
	}

	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		x := line[0]
		y := line[1]
		file := strings.TrimSpace(line[3:])

		if x == '?' {
			result.Untracked = append(result.Untracked, file)
		} else {
			if x != ' ' && x != '?' {
				result.Staged = append(result.Staged, file)
			}
			if y != ' ' && y != '?' {
				result.Modified = append(result.Modified, file)
			}
		}
	}

	return result, nil
}

type LogEntry struct {
	Hash    string `json:"hash"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

type LogResult struct {
	Commits []LogEntry `json:"commits"`
}

func (m *Manager) Log(workDir string, limit int) (*LogResult, error) {
	if workDir == "" {
		return nil, fmt.Errorf("workDir is required")
	}

	format := "%H%n%s%n%an%n%aI"
	out, err := m.run(workDir, "log", fmt.Sprintf("--max-count=%d", limit), fmt.Sprintf("--format=%s", format))
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	result := &LogResult{Commits: []LogEntry{}}

	for i := 0; i+3 < len(lines); i += 4 {
		result.Commits = append(result.Commits, LogEntry{
			Hash:    lines[i][:7],
			Message: lines[i+1],
			Author:  lines[i+2],
			Date:    lines[i+3],
		})
	}

	return result, nil
}

type DiffResult struct {
	Diff string `json:"diff"`
}

func (m *Manager) Diff(workDir, ref string) (*DiffResult, error) {
	if workDir == "" {
		return nil, fmt.Errorf("workDir is required")
	}

	args := []string{"diff"}
	if ref != "" {
		if strings.HasPrefix(ref, "-") {
			return nil, fmt.Errorf("invalid ref: %s", ref)
		}
		args = append(args, ref, "--")
	}

	out, err := m.run(workDir, args...)
	if err != nil {
		return nil, err
	}
	return &DiffResult{Diff: out}, nil
}

type ExecResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (m *Manager) Exec(workDir string, args []string) (*ExecResult, error) {
	if workDir == "" {
		return nil, fmt.Errorf("workDir is required")
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("args is required")
	}

	cmd := exec.Command("git", args...)
	cmd.Dir = workDir

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("failed to execute git: %w", err)
		}
	}

	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func (m *Manager) run(workDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
