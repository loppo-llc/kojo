package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/snapshot"
)

// runRestoreCommand restores a snapshot taken by `kojo --snapshot` into
// the current config directory. Used by docs/snapshot-restore.md §
// "Restore on a backup peer" — the manual `cp` / `rsync` steps the
// runbook prescribed are replaced by a single command that verifies
// the snapshot's sha256 before touching the target.
//
// Operator workflow:
//
//  1. Stop the live kojo on the target ("kojo --restore" refuses to
//     run while the configdir lock is held).
//  2. Run `kojo --restore /path/to/snapshot/<ts>/`. The command
//     verifies manifest + db sha256, then copies kojo.db + blobs/
//     global/ into the target config dir.
//  3. Restore the KEK out-of-band (the snapshot intentionally does
//     not contain it — see docs §3.4).
//  4. Start kojo on the new Hub. Each peer's hub_url config must be
//     updated to point at the new Hub.
//
// Exits 0 on success, 1 on any verification or copy failure. A
// partial-write failure leaves the target in an inconsistent state;
// the operator is told to wipe the dir and retry (the same posture
// the snapshot-side partials get).
func runRestoreCommand(logger *slog.Logger, srcDir, targetConfigDir string, force bool) int {
	if srcDir == "" {
		logger.Error("restore: --restore <snapshot-dir> requires a path")
		return 1
	}
	if targetConfigDir == "" {
		targetConfigDir = configdir.Path()
	}
	// Refuse if the configdir IS a symlink or a non-directory before
	// configdir.Acquire gets a chance to create a lock file inside
	// the followed target. Apply re-checks this for defence in
	// depth; the gate here keeps a planted symlink from materializing
	// kojo.lock on a foreign filesystem.
	if info, err := os.Lstat(targetConfigDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			logger.Error("restore: target config dir is a symlink", "dir", targetConfigDir)
			fmt.Fprintf(os.Stderr, "\ntarget %s is a symlink. Provide the canonical path.\n\n", targetConfigDir)
			return 1
		}
		if !info.IsDir() {
			logger.Error("restore: target config dir is not a directory", "dir", targetConfigDir)
			fmt.Fprintf(os.Stderr, "\ntarget %s is not a directory.\n\n", targetConfigDir)
			return 1
		}
	} else if !os.IsNotExist(err) {
		logger.Error("restore: cannot inspect target config dir", "dir", targetConfigDir, "err", err)
		return 1
	}
	// Acquire the configdir lock so a concurrent `kojo` boot can't
	// interleave with the restore copy. Refuses if the target is
	// already running — operator must stop the live process first.
	lock, err := configdir.Acquire(targetConfigDir)
	if err != nil {
		logger.Error("restore: target config dir is in use by another kojo process",
			"dir", targetConfigDir, "err", err)
		fmt.Fprintf(os.Stderr, "\nStop the running kojo on %s before --restore.\n\n", targetConfigDir)
		return 1
	}
	defer lock.Release()

	opts := snapshot.ApplyOptions{Force: force}
	if err := snapshot.Apply(srcDir, targetConfigDir, opts); err != nil {
		logger.Error("restore: failed", "src", srcDir, "target", targetConfigDir, "err", err)
		fmt.Fprintf(os.Stderr, "\nrestore failed: %v\n\n", err)
		return 1
	}
	logger.Info("restore: complete", "src", srcDir, "target", targetConfigDir)
	fmt.Fprintf(os.Stderr,
		"\nrestore complete.\n  source: %s\n  target: %s\n\nNext steps:\n  1. Restore <target>/auth/kek.bin out-of-band (snapshots intentionally exclude the KEK).\n  2. Start kojo on this host.\n  3. Update each peer's hub_url to point here.\n\n",
		srcDir, targetConfigDir)
	return 0
}
