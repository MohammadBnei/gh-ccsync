package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// toolsFixture adds a committed tools.json (+ RTK.md) and a fake shell whose
// "install"/"update" commands drop an executable into ~/.local/bin.
func toolsFixture(t *testing.T, toolsJSON string) (*Env, *string, *[]string) {
	t.Helper()
	e, choice := fixture(t)
	write(t, filepath.Join(e.Repo, "tools.json"), toolsJSON)
	write(t, filepath.Join(e.Repo, "RTK.md"), "rtk\n")
	must(t, exec.Command("git", "-C", e.Repo, "add", "-A").Run())
	must(t, exec.Command("git", "-C", e.Repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "tools").Run())
	var ran []string
	e.Sh = func(script string, _ io.Writer) error {
		ran = append(ran, script)
		if name, ok := strings.CutPrefix(script, "install "); ok {
			write(t, filepath.Join(e.Home, ".local/bin", name), "#!/bin/sh\n")
			return os.Chmod(filepath.Join(e.Home, ".local/bin", name), 0o755)
		}
		if strings.HasPrefix(script, "update ") {
			return nil
		}
		return errors.New("fail")
	}
	e.ToolMode = ToolsAsk
	*choice = "install"
	return e, choice, &ran
}

const twoTools = `{"rtk": {"install": "install rtk", "update": "update rtk"},
 "node": {"hint": "use nvm"}}`

func TestToolsInstallThenIdempotent(t *testing.T) {
	e, _, ran := toolsFixture(t, twoTools)
	must(t, pass(t, e))
	if !slices.Equal(*ran, []string{"install rtk"}) {
		t.Fatalf("ran %q", *ran)
	}
	if _, err := os.Readlink(filepath.Join(e.Claude, "RTK.md")); err != nil {
		t.Error("RTK.md not linked after rtk was installed in the same run")
	}
	if got := read(t, filepath.Join(e.Cfg, "tools.approved")); got != twoTools {
		t.Error("approved tools.json not recorded")
	}
	*ran = nil
	must(t, pass(t, e))
	if len(*ran) != 0 || e.changes != 0 {
		t.Fatalf("second run not idempotent: ran %q, %d changes\n%s", *ran, e.changes, e.Out)
	}
}

func TestToolsCannedChoiceNeverInstalls(t *testing.T) {
	e, choice, ran := toolsFixture(t, twoTools)
	e.ToolMode = ToolsReport // what main sets when CCSYNC_CHOICE is set or there is no tty
	*choice = "overwrite"
	must(t, pass(t, e))
	if len(*ran) != 0 {
		t.Fatalf("report mode ran %q", *ran)
	}
}

func TestToolsNeverIsRemembered(t *testing.T) {
	e, choice, ran := toolsFixture(t, twoTools)
	*choice = "never"
	must(t, pass(t, e))
	*choice = "install"
	must(t, pass(t, e))
	if len(*ran) != 0 {
		t.Fatalf("declined tool installed by sync: %q", *ran)
	}
	must(t, RunTools(e, []string{"rtk"}))
	if !slices.Equal(*ran, []string{"install rtk"}) {
		t.Fatalf("explicit tools rtk ran %q", *ran)
	}
}

func TestToolsCommandUpdatesAndRejectsUnknown(t *testing.T) {
	e, _, ran := toolsFixture(t, twoTools)
	must(t, pass(t, e))
	*ran = nil
	e.ToolMode = ToolsYes
	must(t, RunTools(e, nil))
	if !slices.Equal(*ran, []string{"update rtk"}) {
		t.Fatalf("update ran %q", *ran)
	}
	if err := RunTools(e, []string{"cavemn"}); !errors.Is(err, errStop) {
		t.Fatalf("unknown name: want errStop, got %v", err)
	}
}

func TestToolsLinuxPrereqs(t *testing.T) {
	e, _, ran := toolsFixture(t, `{"rtk": {"install": "install rtk", "needs": ["curl"]}}`)
	e.OS = "linux"
	e.ToolMode = ToolsYes
	e.Look = func(string) bool { return false } // no curl/tar/sha256sum
	if err := RunTools(e, nil); !errors.Is(err, errStop) {
		t.Fatalf("want errStop without curl, got %v", err)
	}
	if len(*ran) != 0 {
		t.Fatalf("installed without prerequisites: %q", *ran)
	}
}

func TestToolsCheckCatchesWrongBinary(t *testing.T) {
	e, _, ran := toolsFixture(t, `{"rtk": {"install": "install rtk", "check": "rtk-is-wrong"}}`)
	e.Look = func(n string) bool { return n == "rtk" }
	e.ToolMode = ToolsYes
	must(t, pass(t, e))
	if !slices.Contains(*ran, "install rtk") {
		t.Fatalf("a failing check should count as missing: %q", *ran)
	}
}

// check is shell from tools.json: never run before that file is approved.
func TestToolsCheckNotRunUnapproved(t *testing.T) {
	e, _, ran := toolsFixture(t, `{"rtk": {"install": "install rtk", "check": "rtk-is-wrong"}}`)
	e.Look = func(n string) bool { return n == "rtk" }
	e.ToolMode = ToolsReport
	must(t, pass(t, e))
	if len(*ran) != 0 {
		t.Fatalf("ran %q before approval", *ran)
	}
}

func TestToolsWithoutTTYNeedsYes(t *testing.T) {
	e, _, ran := toolsFixture(t, twoTools)
	e.ToolMode = ToolsReport
	if err := RunTools(e, nil); !errors.Is(err, errStop) {
		t.Fatalf("pending steps without --yes: want errStop, got %v", err)
	}
	if len(*ran) != 0 {
		t.Fatalf("ran %q", *ran)
	}
}

func TestPluginsUnreadableStateInstallsNothing(t *testing.T) {
	e, _ := fixture(t)
	write(t, filepath.Join(e.Claude, "settings.json"), `{"enabledPlugins": {"a@m": true},
	  "extraKnownMarketplaces": {"m": {"source": {"source": "github", "repo": "o/m"}}}}`)
	e.Look = func(n string) bool { return n == "claude" }
	e.ToolMode = ToolsYes
	e.Cmd = func(_ string, args ...string) ([]byte, error) {
		if slices.Contains(args, "--json") {
			return []byte("Unknown option --json"), nil
		}
		t.Errorf("ran claude %v with unknown plugin state", args)
		return nil, nil
	}
	must(t, e.checkPlugins(false, nil))
}

func TestCmdForPerOS(t *testing.T) {
	v := map[string]any{"darwin": "brew install x", "linux": "sh tools/x.sh"}
	for goos, want := range map[string]string{"darwin": "brew install x", "linux": "sh tools/x.sh", "freebsd": ""} {
		if got := cmdFor(v, goos); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
	if cmdFor("one", "linux") != "one" {
		t.Error("string command must apply to every OS")
	}
}

func TestPluginsInstallEnabledOnly(t *testing.T) {
	e, _ := fixture(t)
	live := `{"enabledPlugins": {"a@m": true, "b@m": false, "c@g": true, "d@nowhere": true},
	  "extraKnownMarketplaces": {"m": {"source": {"source": "github", "repo": "o/m"}},
	    "g": {"source": {"source": "git", "url": "https://x/g.git"}}}}`
	write(t, filepath.Join(e.Claude, "settings.json"), live)
	installed := map[string]bool{}
	var calls []string
	e.Look = func(n string) bool { return n == "claude" }
	e.Cmd = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch {
		case slices.Equal(args, []string{"plugin", "list", "--json"}):
			var l []string
			for id := range installed {
				l = append(l, `{"id":"`+id+`"}`)
			}
			return []byte("[" + strings.Join(l, ",") + "]"), nil
		case slices.Equal(args, []string{"plugin", "marketplace", "list", "--json"}):
			return []byte("[]"), nil
		case args[1] == "install":
			installed[args[2]] = true
			// the real CLI may touch user settings
			write(t, filepath.Join(e.Claude, "settings.json"), `{"rewritten":true}`)
		}
		return nil, nil
	}
	e.ToolMode = ToolsYes
	must(t, e.checkPlugins(false, nil))
	var acts []string
	for _, c := range calls {
		if !strings.Contains(c, "--json") {
			acts = append(acts, c)
		}
	}
	want := []string{"plugin marketplace add o/m", "plugin install a@m", "plugin marketplace add https://x/g.git", "plugin install c@g"}
	if !slices.Equal(acts, want) {
		t.Fatalf("commands %q\nwant %q", acts, want)
	}
	if read(t, filepath.Join(e.Claude, "settings.json")) != live {
		t.Error("settings.json rewritten by the plugin CLI was not restored")
	}
}

func TestFreshMachineWithoutClaudeDir(t *testing.T) {
	e, choice := fixture(t)
	must(t, os.RemoveAll(e.Claude))
	write(t, filepath.Join(e.Cfg, "host"), "h2\n")
	*choice = "yes"
	must(t, pass(t, e))
	if _, err := os.Stat(filepath.Join(e.Claude, "settings.json")); err != nil {
		t.Fatal("settings.json not written on a machine without ~/.claude")
	}
}
