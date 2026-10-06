package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a home with a committed config repo, live settings that match
// it, and a symlinked skill that must never be touched.
func fixture(t *testing.T) (*Env, *string) {
	t.Helper()
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	files := map[string]string{
		"settings.base.json":         `{"theme":"light"}`,
		"hosts/h1.json":              `{"enabledPlugins":{"a":true}}`,
		"hosts/h1.local.json":        `{"autoMode":{"env":"private"}}`,
		".gitignore":                 "hosts/*.local.json\n",
		"CLAUDE.md":                  "shared\n",
		"statusline.sh":              "echo hi\n",
		"commands/ship.md":           "ship\n",
		"skills/ship/SKILL.md":       "ship skill\n",
		"agents/skills/s1/SKILL.md":  "s1\n",
		"agents/skills/s1/x/more.md": "nested\n",
		"agents/.skill-lock.json":    `{"skills":{}}`,
	}
	for p, c := range files {
		write(t, filepath.Join(repo, p), c)
	}
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", a, out)
		}
	}
	write(t, filepath.Join(home, ".config/claude-config/host"), "h1\n")
	write(t, filepath.Join(home, ".claude/settings.json"), `{"theme":"light","enabledPlugins":{"a":true},"autoMode":{"env":"private"}}`)
	write(t, filepath.Join(home, "elsewhere/retro/SKILL.md"), "retro\n")
	must(t, os.MkdirAll(filepath.Join(home, ".agents/skills"), 0o755))
	must(t, os.Symlink(filepath.Join(home, "elsewhere/retro"), filepath.Join(home, ".agents/skills/retro")))

	choice := "abort"
	var out bytes.Buffer
	e := &Env{
		Repo: repo, Claude: filepath.Join(home, ".claude"), Agents: filepath.Join(home, ".agents"),
		Cfg: filepath.Join(home, ".config/claude-config"), Home: home, Mode: "sync",
		UI: UI{
			Out:     &out,
			Choose:  func(string, ...string) string { return choice },
			Confirm: func(string) bool { return choice != "abort" },
			Input:   func(_, def string) string { return def },
			Spin:    func(_ string, fn func() error) error { return fn() },
		},
		// No real installers, ever: unexpected calls fail the test.
		Sh: func(s string, _ io.Writer) error { t.Errorf("unexpected sh: %s", s); return errors.New("no") },
		Cmd: func(n string, a ...string) ([]byte, error) {
			t.Errorf("unexpected exec: %s %v", n, a)
			return nil, errors.New("no")
		},
		Look: func(string) bool { return false },
	}
	return e, &choice
}

func write(t *testing.T, p, c string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(c), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	must(t, err)
	return string(b)
}

// pass does a fresh run (new counters) on the same paths.
func pass(t *testing.T, e *Env) error {
	t.Helper()
	e.changes, e.backupDir = 0, ""
	return Run(e)
}

func TestFirstRunThenIdempotent(t *testing.T) {
	e, _ := fixture(t)
	must(t, pass(t, e))
	if e.changes == 0 {
		t.Fatal("first run applied nothing")
	}
	for _, p := range []string{"CLAUDE.md", "statusline.sh", "commands/ship.md", "skills/ship"} {
		if tgt, err := os.Readlink(filepath.Join(e.Claude, p)); err != nil || tgt != filepath.Join(e.Repo, p) {
			t.Errorf("%s not linked to repo: %q %v", p, tgt, err)
		}
	}
	if got := read(t, filepath.Join(e.Claude, "skills/s1/x/more.md")); got != "nested\n" {
		t.Errorf("agent skill not reachable through ~/.claude/skills: %q", got)
	}
	if got := read(t, filepath.Join(e.Agents, "skills/retro/SKILL.md")); got != "retro\n" {
		t.Errorf("symlinked skill touched: %q", got)
	}
	must(t, pass(t, e))
	if e.changes != 0 {
		t.Fatalf("second run not idempotent: %d changes\n%s", e.changes, e.Out)
	}
}

func TestSettingsDrift(t *testing.T) {
	e, choice := fixture(t)
	must(t, pass(t, e))
	live := filepath.Join(e.Claude, "settings.json")
	write(t, live, `{"theme":"light","model":"opus","enabledPlugins":{"a":true,"b":true},"autoMode":{"env":"changed"}}`)

	if err := pass(t, e); !errors.Is(err, errStop) {
		t.Fatalf("abort: want errStop, got %v", err)
	}
	if !strings.Contains(read(t, live), "opus") {
		t.Fatal("abort rewrote settings.json")
	}

	*choice = "capture"
	must(t, pass(t, e))
	if !strings.Contains(read(t, filepath.Join(e.Repo, "settings.base.json")), `"model": "opus"`) {
		t.Error("base key not captured into settings.base.json")
	}
	if !strings.Contains(read(t, filepath.Join(e.Repo, "hosts/h1.json")), `"b": true`) {
		t.Error("host key not captured into hosts/h1.json")
	}
	if !strings.Contains(read(t, filepath.Join(e.Repo, "hosts/h1.local.json")), "changed") {
		t.Error("autoMode not captured into hosts/h1.local.json")
	}
	if strings.Contains(read(t, filepath.Join(e.Repo, "hosts/h1.json")), "autoMode") {
		t.Error("autoMode leaked into the committed host file")
	}
}

func TestSettingsOverwriteKeepsBackup(t *testing.T) {
	e, choice := fixture(t)
	must(t, pass(t, e))
	write(t, filepath.Join(e.Claude, "settings.json"), `{"theme":"dark"}`)
	*choice = "overwrite"
	must(t, pass(t, e))
	if !strings.Contains(read(t, filepath.Join(e.Claude, "settings.json")), `"light"`) {
		t.Error("overwrite did not restore the merged settings")
	}
	if !strings.Contains(read(t, filepath.Join(e.backupDir, ".claude/settings.json")), "dark") {
		t.Error("overwritten settings not backed up")
	}
}

func TestAgentsDrift(t *testing.T) {
	e, choice := fixture(t)
	must(t, pass(t, e))
	write(t, filepath.Join(e.Agents, "skills/new/SKILL.md"), "new\n")

	if err := pass(t, e); !errors.Is(err, errStop) {
		t.Fatalf("abort: want errStop, got %v", err)
	}
	*choice = "capture"
	must(t, pass(t, e))
	if read(t, filepath.Join(e.Repo, "agents/skills/new/SKILL.md")) != "new\n" {
		t.Error("new skill not captured")
	}
	if _, err := os.Lstat(filepath.Join(e.Repo, "agents/skills/retro")); err == nil {
		t.Error("symlinked skill captured into the repo")
	}

	// Discard the capture, then overwrite: the extra skill goes, retro stays.
	must(t, exec.Command("git", "-C", e.Repo, "clean", "-fdq", "agents").Run())
	write(t, filepath.Join(e.Agents, "skills/new2/SKILL.md"), "x\n")
	*choice = "overwrite"
	must(t, pass(t, e))
	if _, err := os.Stat(filepath.Join(e.Agents, "skills/new2")); err == nil {
		t.Error("overwrite kept a skill the repo does not have")
	}
	if read(t, filepath.Join(e.Agents, "skills/retro/SKILL.md")) != "retro\n" {
		t.Error("overwrite touched the symlinked skill")
	}
}

func TestLinkDrift(t *testing.T) {
	e, choice := fixture(t)
	must(t, pass(t, e))
	dst := filepath.Join(e.Claude, "CLAUDE.md")
	must(t, os.Remove(dst))
	write(t, dst, "edited locally\n")

	*choice = "skip"
	must(t, pass(t, e))
	if read(t, dst) != "edited locally\n" {
		t.Fatal("skip replaced the file")
	}
	*choice = "replace"
	must(t, pass(t, e))
	if tgt, _ := os.Readlink(dst); tgt != filepath.Join(e.Repo, "CLAUDE.md") {
		t.Error("replace did not relink")
	}
	if read(t, filepath.Join(e.backupDir, ".claude/CLAUDE.md")) != "edited locally\n" {
		t.Error("replaced file not backed up")
	}
}

func TestNewHostOverlay(t *testing.T) {
	e, choice := fixture(t)
	write(t, filepath.Join(e.Cfg, "host"), "h2\n")
	*choice = "yes"
	must(t, pass(t, e))
	h := read(t, filepath.Join(e.Repo, "hosts/h2.json"))
	if !strings.Contains(h, "enabledPlugins") || strings.Contains(h, "autoMode") || strings.Contains(h, "theme") {
		t.Errorf("host overlay has the wrong keys: %s", h)
	}
	if !strings.Contains(read(t, filepath.Join(e.Repo, "hosts/h2.local.json")), "autoMode") {
		t.Error("autoMode not split into the local overlay")
	}
}

func TestDirtyRepoStops(t *testing.T) {
	e, _ := fixture(t)
	write(t, filepath.Join(e.Repo, "CLAUDE.md"), "uncommitted\n")
	if err := pass(t, e); !errors.Is(err, errStop) {
		t.Fatalf("want errStop on a dirty repo, got %v", err)
	}
}

// The manifest must match what `shasum` printed in sync.sh, so switching from
// sync.sh to the binary does not report a false drift.
func TestManifestMatchesShasum(t *testing.T) {
	if _, err := exec.LookPath("shasum"); err != nil {
		t.Skip("no shasum")
	}
	e, _ := fixture(t)
	must(t, pass(t, e))
	out, err := exec.Command("sh", "-c", `cd "$1/skills" && find . -type f -not -path '*/.git/*' | xargs shasum`, "sh", e.Agents).Output()
	must(t, err)
	want := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		want[l] = true
	}
	for _, l := range e.manifest() {
		if !strings.HasSuffix(l, ".skill-lock.json") && !want[l] {
			t.Errorf("manifest line not in shasum output: %q", l)
		}
	}
}

// A machine that was never synced keeps its own skills: merge adds them to the
// repo (and the lock) and deletes nothing; abort changes nothing.
func TestFirstSyncKeepsLocalSkills(t *testing.T) {
	e, choice := fixture(t)
	write(t, filepath.Join(e.Agents, "skills/mine/SKILL.md"), "mine\n")
	write(t, filepath.Join(e.Agents, ".skill-lock.json"), `{"skills":{"mine":{"source":"x"}}}`)

	if err := pass(t, e); !errors.Is(err, errStop) {
		t.Fatalf("abort: want errStop, got %v", err)
	}
	if read(t, filepath.Join(e.Agents, "skills/mine/SKILL.md")) != "mine\n" {
		t.Fatal("abort touched the local skill")
	}

	*choice = "merge"
	must(t, pass(t, e))
	if read(t, filepath.Join(e.Repo, "agents/skills/mine/SKILL.md")) != "mine\n" {
		t.Error("local skill not merged into the repo")
	}
	if read(t, filepath.Join(e.Repo, "agents/skills/s1/SKILL.md")) != "s1\n" {
		t.Error("merge deleted a repo skill")
	}
	if !strings.Contains(read(t, filepath.Join(e.Repo, "agents/.skill-lock.json")), "mine") {
		t.Error("lock entry not merged")
	}
	if read(t, filepath.Join(e.Agents, "skills/mine/SKILL.md")) != "mine\n" {
		t.Error("merge deleted the local skill")
	}
}

func TestClaudeSkillsCaptured(t *testing.T) {
	e, choice := fixture(t)
	write(t, filepath.Join(e.Claude, "skills/handmade/SKILL.md"), "hand\n")
	must(t, os.MkdirAll(filepath.Join(e.Claude, "skills/learned"), 0o755)) // Claude's own store, no SKILL.md
	*choice = "skip"
	must(t, pass(t, e))
	if _, err := os.Stat(filepath.Join(e.Repo, "skills/handmade")); err == nil {
		t.Fatal("skip captured the skill")
	}
	*choice = "capture"
	must(t, pass(t, e))
	if read(t, filepath.Join(e.Repo, "skills/handmade/SKILL.md")) != "hand\n" {
		t.Error("handmade skill not captured")
	}
	if _, err := os.Stat(filepath.Join(e.Repo, "skills/learned")); err == nil {
		t.Error("captured a dir without SKILL.md")
	}
}

func commitAll(t *testing.T, repo string) {
	t.Helper()
	for _, a := range [][]string{{"add", "-A"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "c"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", a, out)
		}
	}
}

// Differing skill: the repo copy wins, the local one is backed up, a .git
// inside a new skill is not copied, stray files are kept, and after the
// commit the next runs are clean and idempotent.
func TestFirstSyncMergeEdgeCases(t *testing.T) {
	e, choice := fixture(t)
	write(t, filepath.Join(e.Agents, "skills/s1/SKILL.md"), "old local s1\n")
	write(t, filepath.Join(e.Agents, "skills/cloned/SKILL.md"), "cloned\n")
	write(t, filepath.Join(e.Agents, "skills/cloned/.git/HEAD"), "ref\n")
	write(t, filepath.Join(e.Agents, "skills/notes.md"), "stray\n")
	*choice = "merge"
	must(t, pass(t, e))
	if read(t, filepath.Join(e.Repo, "agents/skills/s1/SKILL.md")) != "s1\n" {
		t.Error("merge overwrote the repo's s1 with the local copy")
	}
	if read(t, filepath.Join(e.backupDir, ".agents/skills/s1/SKILL.md")) != "old local s1\n" {
		t.Error("local s1 not backed up")
	}
	if _, err := os.Stat(filepath.Join(e.Repo, "agents/skills/cloned/.git")); err == nil {
		t.Error(".git copied into the repo")
	}
	if read(t, filepath.Join(e.Repo, "agents/skills/notes.md")) != "stray\n" {
		t.Error("stray file not merged")
	}
	commitAll(t, e.Repo)
	must(t, pass(t, e))
	if read(t, filepath.Join(e.Agents, "skills/cloned/SKILL.md")) != "cloned\n" {
		t.Error("merged skill lost after the follow-up sync")
	}
	must(t, pass(t, e))
	if e.changes != 0 {
		t.Fatalf("not idempotent after merge: %d changes\n%s", e.changes, e.Out)
	}
}

func TestFirstSyncOverwriteBacksUpLock(t *testing.T) {
	e, choice := fixture(t)
	write(t, filepath.Join(e.Agents, "skills/mine/SKILL.md"), "mine\n")
	write(t, filepath.Join(e.Agents, ".skill-lock.json"), `{"skills":{"mine":{}}}`)
	*choice = "overwrite"
	must(t, pass(t, e))
	if _, err := os.Stat(filepath.Join(e.Agents, "skills/mine")); err == nil {
		t.Error("overwrite kept a skill the repo lacks")
	}
	if read(t, filepath.Join(e.backupDir, ".agents/skills/mine/SKILL.md")) != "mine\n" {
		t.Error("skill not backed up")
	}
	if !strings.Contains(read(t, filepath.Join(e.backupDir, ".agents/.skill-lock.json")), "mine") {
		t.Error("live lock not backed up")
	}
}

func TestDirSumSeesModesAndLinks(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "a/x.sh"), "x\n")
	write(t, filepath.Join(d, "b/x.sh"), "x\n")
	if dirSum(filepath.Join(d, "a")) != dirSum(filepath.Join(d, "b")) {
		t.Fatal("equal dirs hash differently")
	}
	must(t, os.Chmod(filepath.Join(d, "b/x.sh"), 0o755))
	if dirSum(filepath.Join(d, "a")) == dirSum(filepath.Join(d, "b")) {
		t.Error("mode change not seen")
	}
	must(t, os.Symlink("x.sh", filepath.Join(d, "a/l")))
	must(t, os.Symlink("y.sh", filepath.Join(d, "b/l")))
	must(t, os.Chmod(filepath.Join(d, "b/x.sh"), 0o644))
	if dirSum(filepath.Join(d, "a")) == dirSum(filepath.Join(d, "b")) {
		t.Error("link target change not seen")
	}
}
