package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/loppo-llc/kojo/internal/selfupdate"
)

// runUpdateCommand implements `kojo update` and `kojo update -check`.
// It is invoked from main before flag.Parse so the rest of the daemon
// flag surface is never consulted. Exit codes: 0 on success / up-to-date
// / check-only; 1 on any error (including "install requested for an
// unparseable dev build").
func runUpdateCommand(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := fs.Bool("check", false, "check for a newer release and exit without installing")
	if err := fs.Parse(args); err != nil {
		// flag.ExitOnError already exits on parse errors; this is a
		// belt-and-braces path for callers that swap the error handler.
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 1
	}

	// Reap leftover Windows .old binaries before anything else.
	selfupdate.CleanupStaleBinaries()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	client := selfupdate.NewClient(version)
	checker := selfupdate.NewChecker(client, version, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := checker.CheckNow(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update check failed: %v\n", err)
		return 1
	}

	_, parseErr := selfupdate.ParseVersion(version)

	// Plain stdout for humans/scripts; logs stay on stderr via slog.
	fmt.Printf("current: %s\n", st.Current)
	if st.Latest != "" {
		fmt.Printf("latest:  %s\n", st.Latest)
	} else {
		fmt.Printf("latest:  (unknown)\n")
	}
	if st.NotesURL != "" {
		fmt.Printf("notes:   %s\n", st.NotesURL)
	}
	if st.UpdateAvailable {
		fmt.Println("status:  update available")
	} else {
		fmt.Println("status:  up to date")
	}

	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "current version %q is unparseable (dev build); refusing to overwrite with a release binary\n", version)
		if *checkOnly {
			// -check still printed latest above; exit 0 so scripts can
			// poll release tags against a dirty describe stamp.
			return 0
		}
		return 1
	}

	if *checkOnly {
		return 0
	}
	if !st.UpdateAvailable {
		return 0
	}

	applyCtx, applyCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer applyCancel()
	applied, err := checker.Apply(applyCtx)
	if err != nil {
		if errors.Is(err, selfupdate.ErrUpToDate) {
			// Race: something else published "current" between CheckNow
			// and Apply. Treat as success.
			fmt.Println("status:  up to date")
			return 0
		}
		if errors.Is(err, selfupdate.ErrAssetNotFound) {
			fmt.Fprintf(os.Stderr, "release has no binary for %s/%s yet (CI may still be uploading)\n",
				runtime.GOOS, runtime.GOARCH)
			return 1
		}
		if errors.Is(err, selfupdate.ErrApplyInFlight) {
			fmt.Fprintf(os.Stderr, "an update is already in progress\n")
			return 1
		}
		fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
		return 1
	}

	fmt.Printf("installed: %s → %s\n", applied.Current, applied.Latest)
	fmt.Println("the running daemon (if any) keeps the old code until restarted — restart via the web UI, POST /api/v1/system/restart, or relaunch")
	return 0
}
