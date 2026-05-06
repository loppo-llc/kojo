//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// handledAccessFS is the set of filesystem access rights that Landlock will
// govern.  Phase 1 restricts writes only — reads and execution are
// unrestricted because they are not included in this mask.
const handledAccessFS = unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
	unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG |
	unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
	unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
	unix.LANDLOCK_ACCESS_FS_TRUNCATE |
	unix.LANDLOCK_ACCESS_FS_REFER

// Available reports whether the running kernel supports Landlock.
func Available() bool {
	abi, err := landlockABI()
	return err == nil && abi >= 1
}

// wrapCommand builds an exec.Cmd that re-execs kojo with the "sandbox"
// subcommand, passing the allowed RW paths and the real command to execute.
//
// If sandboxing is disabled or unavailable, it falls back to a plain
// exec.CommandContext.
func wrapCommand(ctx context.Context, name string, args []string, cfg Config) *exec.Cmd {
	if !cfg.Enabled || !Available() {
		return exec.CommandContext(ctx, name, args...)
	}

	kojoPath, err := os.Executable()
	if err != nil {
		// Can't find our own binary — fall back to unsandboxed.
		return exec.CommandContext(ctx, name, args...)
	}

	// Build: kojo sandbox --rw path1 --rw path2 -- cmd args...
	sandboxArgs := []string{"sandbox"}
	for _, p := range cfg.RWPaths {
		sandboxArgs = append(sandboxArgs, "--rw", p)
	}
	sandboxArgs = append(sandboxArgs, "--")
	sandboxArgs = append(sandboxArgs, name)
	sandboxArgs = append(sandboxArgs, args...)

	return exec.CommandContext(ctx, kojoPath, sandboxArgs...)
}

// ExecSandboxed is the entry point for the "kojo sandbox" subcommand.
// It parses --rw flags, applies a Landlock ruleset restricting writes to
// the specified paths, then exec's the trailing command.
//
// This function never returns on success (syscall.Exec replaces the process).
// On failure it prints an error and exits.
func ExecSandboxed(args []string) {
	rwPaths, cmdArgs, err := parseSandboxArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kojo sandbox: %v\n", err)
		os.Exit(1)
	}

	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "kojo sandbox: no command specified after --")
		os.Exit(1)
	}

	if err := applyLandlock(rwPaths); err != nil {
		// If Landlock setup fails, abort rather than running unsandboxed.
		// The parent (kojo server) should have checked Available() first;
		// reaching here means an unexpected runtime error.
		fmt.Fprintf(os.Stderr, "kojo sandbox: landlock setup failed: %v\n", err)
		os.Exit(1)
	}

	// Resolve the command to an absolute path (required by syscall.Exec).
	cmdPath, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "kojo sandbox: command not found: %s\n", cmdArgs[0])
		os.Exit(1)
	}

	// Replace this process with the real command.  Landlock restrictions
	// are inherited across exec.
	if err := syscall.Exec(cmdPath, cmdArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "kojo sandbox: exec %s: %v\n", cmdPath, err)
		os.Exit(1)
	}
}

// parseSandboxArgs extracts --rw paths and the trailing command from the
// argument list.  The "--" separator is required between flags and the command.
func parseSandboxArgs(args []string) (rwPaths []string, cmdArgs []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			cmdArgs = args[i+1:]
			return rwPaths, cmdArgs, nil
		}
		if args[i] == "--rw" {
			i++
			if i >= len(args) {
				return nil, nil, fmt.Errorf("--rw requires a path argument")
			}
			rwPaths = append(rwPaths, args[i])
			continue
		}
		if strings.HasPrefix(args[i], "--rw=") {
			rwPaths = append(rwPaths, strings.TrimPrefix(args[i], "--rw="))
			continue
		}
		return nil, nil, fmt.Errorf("unknown flag: %s", args[i])
	}
	return nil, nil, fmt.Errorf("missing -- separator")
}

// landlockABI queries the kernel for the best supported Landlock ABI version.
func landlockABI() (int, error) {
	attr := unix.LandlockRulesetAttr{}
	r, _, errno := syscall.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		0, // size=0 with LANDLOCK_CREATE_RULESET_VERSION flag
		unix.LANDLOCK_CREATE_RULESET_VERSION,
	)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// applyLandlock creates a Landlock ruleset that restricts filesystem writes
// to the given paths, then applies it to the current process.
func applyLandlock(rwPaths []string) error {
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("query ABI: %w", err)
	}
	if abi < 1 {
		return fmt.Errorf("unsupported ABI version %d", abi)
	}

	// Build the handled-access mask, downgrading for older ABIs.
	handled := uint64(handledAccessFS)
	if abi < 2 {
		// REFER added in ABI v2
		handled &^= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi < 3 {
		// TRUNCATE added in ABI v3
		handled &^= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}

	// Prevent privilege escalation via setuid/setgid binaries.
	if err := prctlNoNewPrivs(); err != nil {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", err)
	}

	// Create the ruleset.
	attr := unix.LandlockRulesetAttr{
		Access_fs: handled,
	}
	rulesetFD, _, errno := syscall.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("create_ruleset: %w", errno)
	}
	fd := int(rulesetFD)
	defer syscall.Close(fd)

	// Add a rule for each RW path.
	for _, p := range rwPaths {
		if err := addPathRule(fd, p, handled); err != nil {
			return fmt.Errorf("add rule for %q: %w", p, err)
		}
	}

	// Enforce the ruleset on this process.
	_, _, errno = syscall.Syscall(
		unix.SYS_LANDLOCK_RESTRICT_SELF,
		uintptr(fd),
		0,
		0,
	)
	if errno != 0 {
		return fmt.Errorf("restrict_self: %w", errno)
	}
	return nil
}

// addPathRule adds a Landlock path-beneath rule for the given directory.
func addPathRule(rulesetFD int, path string, accessMask uint64) error {
	pathFD, err := syscall.Open(path, unix.O_PATH|syscall.O_CLOEXEC, 0)
	if err != nil {
		// Path doesn't exist yet — skip silently.  The agent might
		// create it at runtime under an already-allowed parent.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer syscall.Close(pathFD)

	attr := unix.LandlockPathBeneathAttr{
		Allowed_access: accessMask,
		Parent_fd:      int32(pathFD),
	}
	_, _, errno := syscall.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD),
		unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&attr)),
		0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// prctlNoNewPrivs sets PR_SET_NO_NEW_PRIVS on the current thread.
// This is required before landlock_restrict_self and prevents the process
// from gaining privileges via setuid/setgid binaries.
func prctlNoNewPrivs() error {
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PRCTL,
		unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
