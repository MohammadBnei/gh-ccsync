// gh-ccsync syncs a Claude Code config repo into ~/.claude and ~/.agents.
package main

import (
	"errors"
	"fmt"
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
  gh ccsync where                   print the config repo path

CCSYNC_CHOICE=<abort|capture|overwrite|skip|replace> answers prompts without a tty.`

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

	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "", "sync":
	case "capture":
		e.Mode = "capture"
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
	return Run(e)
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
