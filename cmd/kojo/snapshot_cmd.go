package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/snapshot"
	"github.com/loppo-llc/kojo/internal/store"
)

// runSnapshotCommand executes one snapshot and exits. Used by
// `kojo --snapshot` (Phase 5 §3.6) — operators run it from cron / a
// systemd timer to take periodic backups. Refuses to run inside a
// process that already holds the configdir lock; the caller MUST
// NOT pre-acquire the lock when --snapshot is set.
//
// Exits 0 on success. On any error the partially-written snapshot
// directory is left in place so the operator can inspect it; the
// next `kojo --clean snapshots` run (Phase 6 #18) drops
// manifest-less directories.
func runSnapshotCommand(logger *slog.Logger, configDirPath string) int {
	if configDirPath == "" {
		configDirPath = configdir.Path()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	st, err := store.Open(ctx, store.Options{ConfigDir: configDirPath})
	if err != nil {
		logger.Error("snapshot: open store failed", "err", err)
		return 1
	}
	defer st.Close()

	host, _ := os.Hostname()
	dir, err := snapshot.Take(ctx, st, filepath.Join(configDirPath, "blobs"), configDirPath, snapshot.Options{
		HostHint: host,
	})
	if err != nil {
		logger.Error("snapshot: failed", "err", err, "partial", dir)
		return 1
	}
	fmt.Println(dir)
	return 0
}
