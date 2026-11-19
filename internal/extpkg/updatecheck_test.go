package extpkg

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func installDemo(t *testing.T, m *Manager, repo, ref string) Installed {
	t.Helper()
	row, err := m.Install(context.Background(), InstallRequest{URL: repo, Ref: ref, AckScopes: []string{"chat:send"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func bumpDemo(t *testing.T, repo, marker string) string {
	t.Helper()
	writeFiles(t, repo, map[string]string{"skills/demo/SKILL.md": "# demo " + marker + "\n"})
	commitAll(t, repo, marker)
	return gitIn(t, repo, "rev-parse", "HEAD")
}

func TestResolveRemoteRef(t *testing.T) {
	repo := skillsOnlyRepo(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	gitIn(t, repo, "tag", "light")
	gitIn(t, repo, "tag", "-a", "annotated", "-m", "x")
	gitIn(t, repo, "branch", "feature/main")
	next := bumpDemo(t, repo, "next")
	ctx := context.Background()
	dir := t.TempDir()

	cases := []struct {
		ref        string
		want       string
		wantPinned bool
	}{
		{"", next, false},
		{"main", next, false},
		{"refs/heads/main", next, false},
		{"light", head, false},
		// Annotated tags must resolve to the commit, not the tag object.
		{"annotated", head, false},
		{head, "", true},
		{head[:10], "", true},
	}
	for _, c := range cases {
		got, pinned, err := resolveRemoteRef(ctx, dir, repo, c.ref)
		if err != nil {
			t.Fatalf("ref %q: %v", c.ref, err)
		}
		if got != c.want || pinned != c.wantPinned {
			t.Fatalf("ref %q = (%q, %v), want (%q, %v)", c.ref, got, pinned, c.want, c.wantPinned)
		}
	}
	if _, _, err := resolveRemoteRef(ctx, dir, repo, "no-such-branch"); err == nil {
		t.Fatal("missing ref resolved")
	}
}

func TestCheckUpdatesReportsAvailability(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	installDemo(t, m, repo, "")
	ctx := context.Background()

	if _, ok := m.UpdateStatus("demo"); ok {
		t.Fatal("status present before any check")
	}
	m.CheckUpdates(ctx, time.Minute)
	st, ok := m.UpdateStatus("demo")
	if !ok || st.Available || st.CheckedAt == "" || st.Error != "" {
		t.Fatalf("fresh install status = %+v (ok=%v)", st, ok)
	}

	next := bumpDemo(t, repo, "v2")
	m.CheckUpdates(ctx, time.Minute)
	st, _ = m.UpdateStatus("demo")
	if !st.Available || st.Latest != next {
		t.Fatalf("status after remote moved = %+v, want available %s", st, next)
	}
	// Checking never touches the checkout.
	if row, _ := m.Get("demo"); row.Commit == next {
		t.Fatal("check applied the update")
	}

	if _, err := m.Update(ctx, "demo", UpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	st, _ = m.UpdateStatus("demo")
	if st.Available {
		t.Fatalf("still available after manual update: %+v", st)
	}

	if err := m.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if len(m.UpdateStatuses()) != 0 {
		t.Fatal("status survived removal")
	}
}

func TestCheckUpdatesRecordsFailure(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	installDemo(t, m, repo, "main")
	gitIn(t, repo, "branch", "-m", "main", "trunk")
	m.CheckUpdates(context.Background(), time.Minute)
	st, ok := m.UpdateStatus("demo")
	if !ok || st.Error == "" || st.Available {
		t.Fatalf("status = %+v, want a recorded error", st)
	}
}

func TestPinnedRefNeverUpdates(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	row := installDemo(t, m, repo, head)
	if _, err := m.SetAutoUpdate("demo", true); err != nil {
		t.Fatal(err)
	}
	bumpDemo(t, repo, "v2")
	if got := m.CheckAndAutoUpdate(context.Background(), time.Minute, time.Minute); len(got) != 0 {
		t.Fatalf("pinned package updated: %v", got)
	}
	st, _ := m.UpdateStatus("demo")
	if !st.Pinned || st.Available {
		t.Fatalf("status = %+v, want pinned", st)
	}
	if after, _ := m.Get("demo"); after.Commit != row.Commit {
		t.Fatal("pinned checkout moved")
	}
}

func TestAutoUpdateIsOptIn(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	before := installDemo(t, m, repo, "")
	if before.AutoUpdate {
		t.Fatal("auto-update on by default")
	}
	bumpDemo(t, repo, "v2")
	if got := m.CheckAndAutoUpdate(context.Background(), time.Minute, time.Minute); len(got) != 0 {
		t.Fatalf("updated without opt-in: %v", got)
	}
	if after, _ := m.Get("demo"); after.Commit != before.Commit {
		t.Fatal("checkout moved without opt-in")
	}
	st, _ := m.UpdateStatus("demo")
	if !st.Available {
		t.Fatalf("status = %+v, want available", st)
	}
}

func TestAutoUpdateAppliesNewCommit(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	installDemo(t, m, repo, "")
	if _, err := m.SetAutoUpdate("demo", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	next := bumpDemo(t, repo, "v2")
	got := m.CheckAndAutoUpdate(context.Background(), time.Minute, time.Minute)
	if len(got) != 1 || got[0] != "demo" {
		t.Fatalf("updated = %v, want [demo]", got)
	}
	row, _ := m.Get("demo")
	if row.Commit != next || !row.AutoUpdate || !row.Agents["ag_1"].Enabled {
		t.Fatalf("row after auto-update = %+v", row)
	}
	st, _ := m.UpdateStatus("demo")
	if st.Available || st.AutoError != "" {
		t.Fatalf("status after auto-update = %+v", st)
	}
	// Nothing new upstream: the next pass is a no-op.
	if got := m.CheckAndAutoUpdate(context.Background(), time.Minute, time.Minute); len(got) != 0 {
		t.Fatalf("second pass updated %v", got)
	}
}

func TestAutoUpdatePersistsAcrossManagers(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root, "v0.127.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	installDemo(t, m, skillsOnlyRepo(t), "")
	if _, err := m.SetAutoUpdate("demo", true); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(root, "v0.127.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := m2.Get("demo"); !row.AutoUpdate {
		t.Fatal("autoUpdate not persisted")
	}
}

func TestAutoUpdateHoldsScopeChange(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	before := installDemo(t, m, repo, "")
	if _, err := m.SetAutoUpdate("demo", true); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{
		ManifestFilename: strings.Replace(skillsOnlyManifest, `"scopes": ["chat:send"]`, `"scopes": ["chat:send","agents:write"]`, 1),
	})
	commitAll(t, repo, "widen scopes")
	if got := m.CheckAndAutoUpdate(context.Background(), time.Minute, time.Minute); len(got) != 0 {
		t.Fatalf("scope change applied automatically: %v", got)
	}
	row, _ := m.Get("demo")
	if row.Commit != before.Commit || len(row.GrantedScopes) != 1 {
		t.Fatalf("row changed: %+v", row)
	}
	st, _ := m.UpdateStatus("demo")
	if !st.Available || !st.NeedsConsent || st.AutoError == "" {
		t.Fatalf("status = %+v, want held for consent", st)
	}
	// The refused commit is not fetched again on the next pass.
	if ids := m.autoUpdateCandidates(); len(ids) != 0 {
		t.Fatalf("refused commit still a candidate: %v", ids)
	}
	// A later commit is tried again.
	bumpDemo(t, repo, "v3")
	m.CheckUpdates(context.Background(), time.Minute)
	if ids := m.autoUpdateCandidates(); len(ids) != 1 {
		t.Fatalf("new commit not retried: %v", ids)
	}
	// Manual update with consent clears the hold.
	if _, err := m.Update(context.Background(), "demo", UpdateRequest{AckScopes: []string{"chat:send", "agents:write"}}); err != nil {
		t.Fatal(err)
	}
	st, _ = m.UpdateStatus("demo")
	if st.Available || st.NeedsConsent || st.AutoError != "" {
		t.Fatalf("status after consented update = %+v", st)
	}
}

func TestAutoUpdateHoldsIncompatibleKojo(t *testing.T) {
	m := newManager(t) // v0.127.0
	repo := skillsOnlyRepo(t)
	installDemo(t, m, repo, "")
	if _, err := m.SetAutoUpdate("demo", true); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{
		ManifestFilename: strings.Replace(skillsOnlyManifest, `">=0.100.0"`, `">=9.0.0"`, 1),
	})
	commitAll(t, repo, "needs newer kojo")
	if got := m.CheckAndAutoUpdate(context.Background(), time.Minute, time.Minute); len(got) != 0 {
		t.Fatalf("incompatible commit applied: %v", got)
	}
	st, _ := m.UpdateStatus("demo")
	if !st.Available || st.NeedsConsent || !strings.Contains(st.AutoError, "requires kojo") {
		t.Fatalf("status = %+v", st)
	}
	if ids := m.autoUpdateCandidates(); len(ids) != 0 {
		t.Fatalf("incompatible commit still a candidate: %v", ids)
	}
}

func TestUpdateToOtherRefDropsStatus(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	installDemo(t, m, repo, "main")
	gitIn(t, repo, "branch", "other")
	m.CheckUpdates(context.Background(), time.Minute)
	if _, ok := m.UpdateStatus("demo"); !ok {
		t.Fatal("no status after check")
	}
	if _, err := m.Update(context.Background(), "demo", UpdateRequest{Ref: "other"}); err != nil {
		t.Fatal(err)
	}
	if st, ok := m.UpdateStatus("demo"); ok {
		t.Fatalf("status for the old ref survived: %+v", st)
	}
}

func TestUpdateIfVetoKeepsCheckout(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	before := installDemo(t, m, repo, "")
	bumpDemo(t, repo, "v2")
	_, err := m.updateIf(context.Background(), "demo", UpdateRequest{}, func(row *Installed) error {
		if !row.AutoUpdate {
			return errAutoUpdateOff
		}
		return nil
	})
	if err != errAutoUpdateOff {
		t.Fatalf("err = %v, want veto", err)
	}
	if row, _ := m.Get("demo"); row.Commit != before.Commit {
		t.Fatal("vetoed update swapped the checkout")
	}
}

func TestRedactURLErr(t *testing.T) {
	err := redactURLErr(errStr("git ls-remote -- https://user:ghp_secret@github.com/o/r.git main: fatal"))
	if strings.Contains(err.Error(), "ghp_secret") || !strings.Contains(err.Error(), "https://***@github.com/o/r.git") {
		t.Fatalf("redacted = %q", err)
	}
	plain := errStr("git ls-remote -- git@github.com:o/r.git main: fatal")
	if got := redactURLErr(plain); got.Error() != plain.Error() {
		t.Fatalf("scp-style URL altered: %q", got)
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }

func TestCheckUpdatesGivesUpWhenBusy(t *testing.T) {
	m := newManager(t)
	m.checkSem <- struct{}{} // a pass is running
	defer m.releaseCheck()
	if err := m.CheckUpdates(context.Background(), time.Minute); !errors.Is(err, ErrCheckBusy) {
		t.Fatalf("err = %v, want ErrCheckBusy", err)
	}
}
