// gh-ccsync syncs a Claude Code config repo into ~/.claude and ~/.agents.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const usage = `gh ccsync — sync a Claude Code config repo into ~/.claude and ~/.agents

  gh ccsync                         pull, then apply (asks on local drift)
  gh ccsync capture                 copy local changes back into the repo
  gh ccsync init <owner/repo> [dir] clone the config repo (default ~/Code/claude-config), then apply
  gh ccsync tools [--yes] [name…]   install missing tools/plugins, update present ones
  gh ccsync where                   print the config repo path

CCSYNC_CHOICE=<abort|capture|overwrite|skip|replace> answers prompts without a tty.
It never installs tools: without a tty only "gh ccsync tools --yes" does.`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, errStop) {
			fmt.Fprintln(os.Stderr, sWarn.Render("error: "+err.Error()))
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if runtime.GOOS == "windows" {
		return errors.New("native Windows is not supported (symlinks need admin); run inside WSL")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	cfg := filepath.Join(home, ".config", "claude-config")
	e := &Env{Claude: claude, Agents: filepath.Join(home, ".agents"), Cfg: cfg, Home: home, Mode: "sync", UI: terminalUI()}
	e.Look = func(n string) bool { _, err := exec.LookPath(n); return err == nil }
	e.Cmd = func(n string, a ...string) ([]byte, error) { return exec.Command(n, a...).CombinedOutput() }
	e.ToolMode = ToolsReport
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("CCSYNC_CHOICE") == "" {
		e.ToolMode = ToolsAsk
	}
	var toolNames []string

	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "", "sync":
	case "capture":
		e.Mode = "capture"
	case "tools":
		e.Mode = "tools"
		for _, a := range args[1:] {
			if a == "--yes" || a == "-y" {
				e.ToolMode = ToolsYes
			} else {
				toolNames = append(toolNames, a)
			}
		}
	case "init":
		if len(args) < 2 {
			return errors.New("usage: gh ccsync init <owner/repo> [dir]")
		}
		dir := filepath.Join(home, "Code", "claude-config")
		if len(args) > 2 {
			dir = args[2]
		}
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			c := exec.Command("gh", "repo", "clone", args[1], dir)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("clone %s: %w", args[1], err)
			}
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cfg, "repo"), []byte(abs+"\n"), 0o644); err != nil {
			return err
		}
	case "where":
		r, err := repoPath(cfg, home)
		if err == nil {
			fmt.Println(r)
		}
		return err
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		fmt.Fprintln(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}

	if e.Repo, err = repoPath(cfg, home); err != nil {
		return err
	}
	e.Sh = shRunner(e.Repo, home, e.ToolMode == ToolsYes)
	if e.Mode == "tools" {
		return RunTools(e, toolNames)
	}
	return Run(e)
}

// shRunner runs tool commands from the repo (so tools.json can call tools/*.sh)
// with ~/.local/bin on PATH, $SUDO empty for root, and apt kept non-interactive.
// With --yes stdin is /dev/null, so nothing can wait for input.
func shRunner(repo, home string, yes bool) func(string, io.Writer) error {
	sudo := "sudo"
	if os.Geteuid() == 0 {
		sudo = ""
	}
	return func(script string, out io.Writer) error {
		c := exec.Command("sh", "-c", script)
		c.Dir = repo
		c.Env = append(os.Environ(), "SUDO="+sudo, "DEBIAN_FRONTEND=noninteractive",
			"PATH="+filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		if !yes {
			c.Stdin = os.Stdin
		}
		c.Stdout, c.Stderr = out, out
		return c.Run()
	}
}

// repoPath: the path saved by init, else the conventional checkout.
func repoPath(cfg, home string) (string, error) {
	if b, err := os.ReadFile(filepath.Join(cfg, "repo")); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	def := filepath.Join(home, "Code", "claude-config")
	if _, err := os.Stat(filepath.Join(def, "settings.base.json")); err == nil {
		return def, nil
	}
	return "", errors.New("no config repo; run: gh ccsync init <owner/repo>")
}
