//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// minLandlockABI is the minimum Landlock ABI version we require for the
// sandbox to be considered active. ABI v3 (Linux 6.2+) added the
// LANDLOCK_ACCESS_FS_TRUNCATE right; without it, an agent inside the
// sandbox could still truncate any file the kernel exposes for write —
// including files in other agents' data directories. Falling back to
// v1/v2 with TRUNCATE dropped from the handled mask would silently
// degrade write isolation while Available() still reported "sandboxed",
// so we fail closed instead.
const minLandlockABI = 3

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

// Available reports whether the running kernel supports a Landlock ABI
// strong enough for kojo's write-isolation guarantees.
//
// We require ABI >= minLandlockABI (currently 3) so that
// LANDLOCK_ACCESS_FS_TRUNCATE is honoured. Older ABIs would let an agent
// truncate files outside the allowlist, which defeats the per-agent
// write-isolation this package exists to provide.
func Available() bool {
	abi, err := landlockABI()
	return err == nil && abi >= minLandlockABI
}

// wrapCommand builds an exec.Cmd that re-execs kojo with the "sandbox"
// subcommand, passing the allowed RW paths and the real command to execute.
//
// Fail-closed contract:
//   - cfg.Enabled == false  → passthrough (caller deliberately opted out).
//   - cfg.Enabled == true   → either return a real sandbox-wrapped Cmd,
//     OR return a Cmd whose Start() fails. We never silently fall back to
//     an unsandboxed exec, because the caller asked for sandboxing and a
//     transparent downgrade would hide a material security posture change
//     behind a successful-looking process launch.
//
// Failures handled this way:
//   - Available() flipped to false between sandboxConfig and now (TOCTOU
//     against module-level state or kernel module unload).
//   - os.Executable() can't resolve the kojo binary (deleted, exec on a
//     stripped layer, etc.) — without a kojo binary we can't re-exec
//     ourselves as the sandbox helper.
//
// Both are returned through *exec.Cmd's Err field; exec.Cmd.Start propagates
// it directly so the caller sees a clear "sandbox setup failed" instead of
// an unsandboxed agent silently coming up.
func wrapCommand(ctx context.Context, name string, args []string, cfg Config) *exec.Cmd {
	if !cfg.Enabled {
		return exec.CommandContext(ctx, name, args...)
	}

	if !Available() {
		return errorCmd(ctx, name, args, fmt.Errorf(
			"sandbox: Enabled=true but Landlock is no longer available — refusing to launch unsandboxed"))
	}

	kojoPath, err := os.Executable()
	if err != nil {
		return errorCmd(ctx, name, args, fmt.Errorf(
			"sandbox: cannot resolve kojo executable for re-exec (%w) — refusing to launch unsandboxed", err))
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

// errorCmd returns an *exec.Cmd whose Start() will fail with err. The Cmd
// still carries the original name/args so callers that log cmd.Path/Args on
// failure produce intelligible diagnostics. exec.Cmd.Err was added precisely
// for this "constructor failure" use case — Start() returns it without
// attempting fork/exec.
func errorCmd(ctx context.Context, name string, args []string, err error) *exec.Cmd {
	c := exec.CommandContext(ctx, name, args...)
	c.Err = err
	return c
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

// landlockABI queries the kernel for the best supported Landlock ABI
// version.
//
// The version-probe form of landlock_create_ruleset requires BOTH the attr
// pointer to be NULL AND size to be 0. The kernel checks this explicitly
// (see security/landlock/syscalls.c: the LANDLOCK_CREATE_RULESET_VERSION
// branch returns -EINVAL unless `!attr && !size`). An earlier revision of
// this function passed a non-nil pointer to a zero-value struct, which
// looks harmless but is treated by the kernel as a malformed call:
// landlockABI always returned EINVAL on every supported kernel, Available()
// fell through to false, and the sandbox was never enabled in practice.
// The NULL form below is the only form that actually returns the ABI.
func landlockABI() (int, error) {
	r, _, errno := syscall.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0, // attr must be NULL for the version probe
		0, // size must be 0 for the version probe
		unix.LANDLOCK_CREATE_RULESET_VERSION,
	)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// applyLandlock creates a Landlock ruleset that restricts filesystem writes
// to the given paths, then applies it to the current process.
//
// Important: this function locks the calling goroutine to its OS thread and
// never unlocks it. prctl(PR_SET_NO_NEW_PRIVS) and landlock_restrict_self
// are both thread-scoped on Linux. If the Go runtime rescheduled the
// goroutine between those two syscalls, restrict_self could fail with EPERM
// because the new thread does not have no_new_privs set. The kojo sandbox
// process is single-purpose (it calls applyLandlock then syscall.Exec to
// replace itself with the agent binary), so leaving the thread locked has
// no downside.
func applyLandlock(rwPaths []string) error {
	runtime.LockOSThread()

	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("query ABI: %w", err)
	}
	if abi < minLandlockABI {
		// Fail closed. Older ABIs (v1, v2) lack LANDLOCK_ACCESS_FS_TRUNCATE,
		// so an agent inside the sandbox could still truncate files outside
		// the allowlist. We refuse to provide a weaker-than-advertised
		// guarantee — Available() should have gated this call already.
		return fmt.Errorf("Landlock ABI %d is below required minimum %d "+
			"(missing LANDLOCK_ACCESS_FS_TRUNCATE); refusing to enforce a "+
			"sandbox that cannot block write isolation bypasses", abi, minLandlockABI)
	}

	// Full handled-access mask. ABI v3+ supports every right we care
	// about (WRITE_FILE, REMOVE_*, MAKE_*, TRUNCATE, REFER), so no
	// downgrading is needed.
	handled := uint64(handledAccessFS)

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
		// Path doesn't exist. Once Landlock is enforced, the agent can
		// only create a directory if one of its ancestors is already on
		// the allowlist. Most kojo-owned paths (agent data dir, the tool
		// config dirs we deliberately allow) do not have such an ancestor
		// — silently skipping here means the CLI would then fail to
		// create them at runtime with EACCES.
		//
		// Surface the issue on stderr (the kojo sandbox subprocess
		// streams stderr to the parent) so operators can diagnose
		// "claude can't write to ~/.claude on a fresh machine". The
		// caller (sandboxConfig in internal/agent) is expected to
		// pre-create kojo-owned paths before invoking us; this log
		// catches any path we missed.
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr,
				"kojo sandbox: skipping missing RW path %q — the sandboxed "+
					"process will not be able to create it unless an ancestor "+
					"is also on the allowlist\n", path)
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
