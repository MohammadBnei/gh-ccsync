package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// Tools: binaries listed in the repo's tools.json and plugins enabled in
// settings. Nothing runs without consent: ToolsAsk confirms on a tty,
// ToolsYes is `gh ccsync tools --yes`, ToolsReport only lists.
const (
	ToolsAsk    = "ask"
	ToolsYes    = "yes"
	ToolsReport = "report"
)

var errTools = errors.New("a tool step failed")

type tool struct {
	Install any      `json:"install"` // a command, or {goos: command}
	Update  any      `json:"update"`
	Check   string   `json:"check"` // optional: proves it is the right binary
	Hint    string   `json:"hint"`  // shown when there is no command for this OS
	Needs   []string `json:"needs"` // binaries the commands use (e.g. curl), checked first
}

type action struct {
	name, show string
	run        func() error
	verify     func() bool // nil: the exit code is enough
}

func cmdFor(v any, goos string) string {
	switch c := v.(type) {
	case string:
		return c
	case map[string]any:
		s, _ := c[goos].(string)
		return s
	}
	return ""
}

func (e *Env) goos() string {
	if e.OS != "" {
		return e.OS
	}
	return runtime.GOOS
}

func (e *Env) look(name string) bool { return e.Look != nil && e.Look(name) }

func (e *Env) sh(script string, out io.Writer) error {
	if e.Sh == nil {
		return errors.New("no shell runner configured")
	}
	return e.Sh(script, out)
}

func (e *Env) cmd(name string, args ...string) ([]byte, error) {
	if e.Cmd == nil {
		return nil, errors.New("no command runner configured")
	}
	return e.Cmd(name, args...)
}

// have finds a binary on PATH, or in ~/.local/bin where the Linux installers
// put it before a new login shell picks that dir up.
func (e *Env) have(name string) (ok, offPath bool) {
	if e.look(name) {
		return true, false
	}
	fi, err := os.Stat(filepath.Join(e.Home, ".local", "bin", name))
	return err == nil && fi.Mode()&0o111 != 0, true
}

func readSet(p string) map[string]bool {
	s := map[string]bool{}
	l, _ := readLines(p)
	for _, v := range l {
		s[v] = true
	}
	return s
}

func writeSet(p string, s map[string]bool) error {
	var l []string
	for k, v := range s {
		if v {
			l = append(l, k)
		}
	}
	sort.Strings(l)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strings.Join(l, "\n")+"\n"), 0o644)
}

// match reports whether name is selected, and marks it seen.
func match(want map[string]bool, name string) bool {
	if want == nil {
		return true
	}
	if _, ok := want[name]; !ok {
		return false
	}
	want[name] = true
	return true
}

// checkBinaries installs missing tools.json binaries; with update it also
// runs the update command of the present ones.
func (e *Env) checkBinaries(update bool, want map[string]bool) error {
	e.step("Tools")
	p := filepath.Join(e.Repo, "tools.json")
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		e.info("no tools.json")
		return nil
	}
	if err != nil {
		return err
	}
	var tools map[string]tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		e.warn("tools.json: %v", err)
		return errTools
	}
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)

	declinedFile := filepath.Join(e.Cfg, "tools.declined")
	declined := readSet(declinedFile)
	goos := e.goos()
	var acts []action
	offPath, failedPre := false, false
	for _, n := range names {
		if !match(want, n) {
			continue
		}
		t := tools[n]
		if want != nil && declined[n] {
			declined[n] = false // naming it explicitly clears "never"
			_ = writeSet(declinedFile, declined)
		}
		ok, off := e.have(n)
		if ok && t.Check != "" && e.sh(t.Check, io.Discard) != nil {
			e.warn("%s found, but `%s` fails (wrong binary?)", n, t.Check)
			ok = false
		}
		verify := func() bool {
			ok, off := e.have(n)
			offPath = offPath || (ok && off)
			return ok
		}
		switch {
		case ok && update && cmdFor(t.Update, goos) != "":
			acts = append(acts, e.shAction(n, cmdFor(t.Update, goos), verify))
		case ok:
			e.ok("%s", n)
			offPath = offPath || off
		case declined[n]:
			e.info("%s missing (declined on this host; `gh ccsync tools %s` to install)", n, n)
		case cmdFor(t.Install, goos) == "":
			hint := t.Hint
			if hint == "" {
				hint = "no install command for " + goos
			}
			e.warn("%s missing: %s", n, hint)
		default:
			acts = append(acts, e.shAction(n, cmdFor(t.Install, goos), verify))
		}
		if len(acts) > 0 && acts[len(acts)-1].name == n {
			if miss := e.missing(t.Needs); len(miss) > 0 {
				e.warn("%s needs %s first%s", n, strings.Join(miss, ", "), aptHint(goos, miss))
				acts = acts[:len(acts)-1]
				failedPre = true
			}
		}
	}
	if len(acts) == 0 {
		if failedPre {
			return errTools
		}
		return e.offPathNote(offPath)
	}

	approved := filepath.Join(e.Cfg, "tools.approved")
	if prev, err := os.ReadFile(approved); err == nil && !bytes.Equal(prev, raw) && e.ToolMode == ToolsAsk {
		e.warn("tools.json changed since you last approved it:")
		e.showDiff(prev, raw)
	}
	run, err := e.consent("tools", acts, update, declinedFile)
	if !run || err != nil {
		return err
	}
	if err := os.MkdirAll(e.Cfg, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(approved, raw, 0o644); err != nil {
		return err
	}
	err = e.apply(acts)
	_ = e.offPathNote(offPath)
	if err == nil && failedPre {
		err = errTools
	}
	return err
}

func (e *Env) missing(bins []string) []string {
	var m []string
	for _, b := range bins {
		if !e.look(b) {
			m = append(m, b)
		}
	}
	return m
}

// aptHint names the Debian/Ubuntu packages for the usual prerequisites.
func aptHint(goos string, bins []string) string {
	if goos != "linux" {
		return ""
	}
	pkg := map[string]string{"curl": "curl ca-certificates", "sha256sum": "coreutils"}
	var p []string
	for _, b := range bins {
		if v, ok := pkg[b]; ok {
			p = append(p, v)
		} else {
			p = append(p, b)
		}
	}
	sudo := "sudo "
	if os.Geteuid() == 0 {
		sudo = ""
	}
	return ": " + sudo + "apt-get install -y " + strings.Join(p, " ")
}

func (e *Env) offPathNote(offPath bool) error {
	if offPath {
		e.warn("~/.local/bin is not on this shell's PATH: open a new shell (exec $SHELL -l) before starting claude")
	}
	return nil
}

func (e *Env) shAction(name, c string, verify func() bool) action {
	return action{name: name, show: c, verify: verify, run: func() error { return e.sh(c, e.Out) }}
}

func (e *Env) cmdAction(name string, args ...string) action {
	return action{name: name, show: "claude " + strings.Join(args, " "), run: func() error {
		out, err := e.cmd("claude", args...)
		e.Out.Write(out)
		return err
	}}
}

// consent shows the exact commands and asks. Only ToolsAsk on a tty or
// ToolsYes runs anything; a canned CCSYNC_CHOICE never installs.
func (e *Env) consent(what string, acts []action, update bool, declinedFile string) (bool, error) {
	if e.ToolMode != ToolsYes { // with --yes, apply echoes each command as it runs
		for _, a := range acts {
			fmt.Fprintln(e.Out, sInfo.Render("    $ "+a.show))
		}
	}
	switch e.ToolMode {
	case ToolsYes:
		return true, nil
	case ToolsAsk:
	default:
		e.warn("%d %s step(s) pending: run `gh ccsync tools` to apply", len(acts), what)
		return false, nil
	}
	if update {
		return e.Confirm(fmt.Sprintf("Run these %d command(s)?", len(acts))), nil
	}
	switch e.Choose(fmt.Sprintf("Install %d missing %s?", len(acts), what), "skip", "install", "never") {
	case "install":
		return true, nil
	case "never":
		d := readSet(declinedFile)
		for _, a := range acts {
			d[a.name] = true
		}
		e.info("won't ask again on this host; `gh ccsync tools <name>` installs it")
		return false, writeSet(declinedFile, d)
	}
	return false, nil
}

func (e *Env) apply(acts []action) error {
	failed := 0
	for _, a := range acts {
		e.info("$ %s", a.show)
		if err := a.run(); err != nil {
			e.warn("%s failed: %v", a.name, err)
			failed++
			continue
		}
		if a.verify != nil && !a.verify() {
			e.warn("%s: command succeeded but the binary is still not found", a.name)
			failed++
			continue
		}
		e.changes++
		e.ok("%s", a.name)
	}
	if failed > 0 {
		return errTools
	}
	return nil
}

// --- plugins --------------------------------------------------------------

func (e *Env) jsonList(field string, args ...string) map[string]bool {
	s := map[string]bool{}
	out, err := e.cmd("claude", args...)
	if err != nil {
		return s
	}
	var l []map[string]any
	_ = json.Unmarshal(out, &l)
	for _, m := range l {
		if v, ok := m[field].(string); ok {
			s[v] = true
		}
	}
	return s
}

// marketSource is the argument `claude plugin marketplace add` takes for a
// declared extraKnownMarketplaces entry, or "" for an unknown shape.
func marketSource(extra map[string]any, mkt string) string {
	m, _ := extra[mkt].(map[string]any)
	src, _ := m["source"].(map[string]any)
	kind, _ := src["source"].(string)
	key := map[string]string{"github": "repo", "git": "url", "url": "url", "directory": "path"}[kind]
	v, _ := src[key].(string)
	return v
}

// checkPlugins installs the enabledPlugins (true only) that are missing, and
// with update refreshes marketplaces and plugins. The plugin CLI may rewrite
// settings.json; ccsync owns that file, so it is restored afterwards.
func (e *Env) checkPlugins(update bool, want map[string]bool) error {
	e.step("Plugins")
	if !e.look("claude") {
		e.info("claude not on PATH, skipping plugins")
		return nil
	}
	live := filepath.Join(e.Claude, "settings.json")
	s, err := readObj(live)
	if err != nil {
		e.warn("plugins: %v", err)
		return nil
	}
	enabled, _ := s["enabledPlugins"].(map[string]any)
	extra, _ := s["extraKnownMarketplaces"].(map[string]any)
	var ids []string
	for id, v := range enabled {
		if b, _ := v.(bool); b && match(want, id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}

	declinedFile := filepath.Join(e.Cfg, "tools.declined")
	declined := readSet(declinedFile)
	installed := e.jsonList("id", "plugin", "list", "--json")
	markets := e.jsonList("name", "plugin", "marketplace", "list", "--json")
	var acts []action
	for _, id := range ids {
		_, mkt, _ := strings.Cut(id, "@")
		switch {
		case installed[id] && update:
			acts = append(acts, e.cmdAction(id, "plugin", "update", id))
		case installed[id]:
			e.ok("%s", id)
		case declined[id] && want == nil:
			e.info("%s missing (declined on this host)", id)
		default:
			if !markets[mkt] {
				src := marketSource(extra, mkt)
				if src == "" {
					e.warn("%s: marketplace %q is not declared in extraKnownMarketplaces, skipped", id, mkt)
					continue
				}
				acts = append(acts, e.cmdAction("marketplace "+mkt, "plugin", "marketplace", "add", src))
				markets[mkt] = true
			}
			a := e.cmdAction(id, "plugin", "install", id)
			a.verify = func() bool { return e.jsonList("id", "plugin", "list", "--json")[id] }
			acts = append(acts, a)
		}
	}
	if len(acts) == 0 {
		return nil
	}
	if update {
		acts = append([]action{e.cmdAction("marketplaces", "plugin", "marketplace", "update")}, acts...)
	}
	run, err := e.consent("plugins", acts, update, declinedFile)
	if !run || err != nil {
		return err
	}

	before, _ := os.ReadFile(live)
	err = e.apply(acts)
	if after, _ := os.ReadFile(live); !bytes.Equal(before, after) {
		e.warn("the plugin CLI rewrote settings.json; restoring the synced version")
		if berr := e.backup(live); berr != nil {
			return berr
		}
		if werr := os.WriteFile(live, before, 0o644); werr != nil {
			return werr
		}
	}
	return err
}

// RunTools is `gh ccsync tools [--yes] [name…]`: install missing, update present.
func RunTools(e *Env, names []string) error {
	host, err := e.hostID()
	if err != nil {
		return err
	}
	e.box(sTitle, "claude-config · tools", "host: "+host, "repo: "+e.short(e.Repo))
	if err := e.syncRepo(); err != nil {
		return err
	}
	var want map[string]bool
	if len(names) > 0 {
		want = map[string]bool{}
		for _, n := range names {
			want[n] = false
		}
	}
	errB := e.checkBinaries(true, want)
	errP := e.checkPlugins(true, want)
	var unknown []string
	for n, seen := range want {
		if !seen {
			unknown = append(unknown, n)
		}
	}
	slices.Sort(unknown)
	if len(unknown) > 0 {
		e.warn("unknown tool or plugin: %s", strings.Join(unknown, ", "))
		return errStop
	}
	if errB != nil || errP != nil {
		return errStop
	}
	fmt.Fprintln(e.Out)
	e.box(sDone, "✓ tools done")
	return nil
}
