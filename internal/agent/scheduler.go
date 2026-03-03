package agent

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheduler runs agent sessions on configured schedules.
type Scheduler struct {
	mu      sync.Mutex
	logger  *slog.Logger
	manager *Manager
	stopCh  chan struct{}
	refresh chan struct{}
	stopped bool
}

// NewScheduler creates a new scheduler.
func NewScheduler(logger *slog.Logger, manager *Manager) *Scheduler {
	return &Scheduler{
		logger:  logger,
		manager: manager,
		stopCh:  make(chan struct{}),
		refresh: make(chan struct{}, 1),
	}
}

// Start begins the scheduler loop.
func (s *Scheduler) Start() {
	go s.loop()
}

// Stop halts the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
}

// Refresh signals the scheduler to recompute next run times.
func (s *Scheduler) Refresh() {
	select {
	case s.refresh <- struct{}{}:
	default:
	}
}

func (s *Scheduler) loop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-s.refresh:
			s.check()
		case <-ticker.C:
			s.check()
		}
	}
}

func (s *Scheduler) check() {
	agents := s.manager.List()
	now := time.Now()

	for _, a := range agents {
		if !a.Enabled || a.Schedule == "" {
			continue
		}
		if s.manager.IsRunning(a.ID) {
			continue
		}
		if !shouldRun(a, now) {
			continue
		}
		s.logger.Info("scheduler triggering agent", "id", a.ID, "name", a.Name)
		go func(agentID, agentName string) {
			if _, err := s.manager.Run(agentID); err != nil {
				s.logger.Warn("scheduler run failed", "id", agentID, "err", err)
			}
		}(a.ID, a.Name)
	}
}

// shouldRun determines if an agent should be triggered based on its schedule and last run time.
func shouldRun(a *Agent, now time.Time) bool {
	schedule := strings.TrimSpace(a.Schedule)
	if schedule == "" {
		return false
	}

	var lastRun time.Time
	if a.LastRunAt != "" {
		t, err := time.Parse(time.RFC3339, a.LastRunAt)
		if err == nil {
			lastRun = t
		}
	}

	interval, err := parseSchedule(schedule)
	if err != nil {
		return false
	}

	if lastRun.IsZero() {
		return true // never run before
	}

	return now.Sub(lastRun) >= interval
}

// parseSchedule parses schedule strings like "hourly", "every 6h", "daily 09:00".
func parseSchedule(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)

	switch s {
	case "hourly":
		return time.Hour, nil
	}

	if strings.HasPrefix(s, "daily") {
		return 24 * time.Hour, nil
	}

	if strings.HasPrefix(s, "every ") {
		rest := strings.TrimPrefix(s, "every ")
		rest = strings.TrimSpace(rest)
		if strings.HasSuffix(rest, "h") {
			n, err := strconv.Atoi(strings.TrimSuffix(rest, "h"))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid schedule: %s", s)
			}
			return time.Duration(n) * time.Hour, nil
		}
		if strings.HasSuffix(rest, "m") {
			n, err := strconv.Atoi(strings.TrimSuffix(rest, "m"))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid schedule: %s", s)
			}
			return time.Duration(n) * time.Minute, nil
		}
	}

	return 0, fmt.Errorf("unrecognized schedule: %s", s)
}
