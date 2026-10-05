package main

import (
	"bytes"
	"errors"
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
	if !strings.Contains(read(t, filepath.Join(e.backupDir, "settings.json")), "dark") {
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
	if read(t, filepath.Join(e.backupDir, "CLAUDE.md")) != "edited locally\n" {
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
