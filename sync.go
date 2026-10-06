package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

var (
	hostKeys  = []string{"enabledPlugins", "pluginConfigs", "hooks"} // committed per host
	localKeys = []string{"autoMode"}                                 // per host, never committed

	errStop = errors.New("stopped") // message already shown; exit 1
)

// Env holds every path the sync touches, so tests can point it at a temp dir.
type Env struct {
	Repo   string // the claude-config checkout
	Claude string // ~/.claude
	Agents string // ~/.agents
	Cfg    string // ~/.config/claude-config
	Home   string // only for shortening paths in output
	Mode   string // "sync" or "capture"
	UI

	// External commands, injected so tests never run real ones. nil refuses.
	Sh        func(script string, out io.Writer) error // sh -c, cwd = Repo
	Cmd       func(name string, args ...string) ([]byte, error)
	Look      func(name string) bool
	ToolMode  string // ToolsAsk, ToolsYes or ToolsReport (default: report)
	OS        string // GOOS override for tests
	backupDir string
	changes   int
	pending   int // tool steps listed but not run (no consent)
}

func (e *Env) short(p string) string {
	if e.Home != "" && strings.HasPrefix(p, e.Home) {
		return "~" + strings.TrimPrefix(p, e.Home)
	}
	return p
}

func (e *Env) rel(p string) string { r, _ := filepath.Rel(e.Repo, p); return r }

// pick returns the drift answer: capture mode always captures.
func (e *Env) pick(header string, opts ...string) string {
	if e.Mode == "capture" {
		return "capture"
	}
	return e.Choose(header, opts...)
}

func (e *Env) backup(p string) error {
	if e.backupDir == "" {
		e.backupDir = filepath.Join(e.Claude, "backups", "sync-"+time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(e.backupDir, 0o755); err != nil {
		return err
	}
	rel := filepath.Base(p)
	if r, err := filepath.Rel(e.Home, p); err == nil && !strings.HasPrefix(r, "..") {
		rel = r // ~/.agents/skills/x and ~/.claude/skills/x must not collide
	}
	dst := filepath.Join(e.backupDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	e.info("backed up %s → %s", e.short(p), e.short(dst))
	return os.Rename(p, dst)
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// showDiff prints a colored diff of two byte slices via git, which every host has.
func (e *Env) showDiff(a, b []byte) {
	dir, err := os.MkdirTemp("", "ccsync")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)
	pa, pb := filepath.Join(dir, "expected"), filepath.Join(dir, "live")
	_ = os.WriteFile(pa, a, 0o644)
	_ = os.WriteFile(pb, b, 0o644)
	out, _ := exec.Command("git", "--no-pager", "diff", "--no-index", "--color=always", pa, pb).Output()
	e.Out.Write(out)
}

// Run is one full sync (or capture) pass.
func Run(e *Env) error {
	host, err := e.hostID()
	if err != nil {
		return err
	}
	e.box(sTitle, "claude-config · "+e.Mode, "host: "+host, "repo: "+e.short(e.Repo))

	if _, err := os.Stat(filepath.Join(e.Repo, ".githooks")); err == nil {
		_, _ = git(e.Repo, "config", "core.hooksPath", ".githooks")
	}
	if err := e.syncRepo(); err != nil {
		return err
	}
	// Tools before links (RTK.md needs rtk); a tool problem never fails the sync.
	if e.Mode == "sync" {
		_ = e.checkBinaries(false, nil)
	}
	if done, err := e.syncSettings(host); done || err != nil {
		return err
	}
	if e.Mode == "sync" {
		_ = e.checkPlugins(false, nil)
	}
	if err := e.syncLinks(); err != nil {
		return err
	}
	if done, err := e.syncAgents(); done || err != nil {
		return err
	}
	if done, err := e.syncClaudeSkills(); done || err != nil {
		return err
	}

	msg := "no changes"
	if e.changes > 0 {
		msg = fmt.Sprintf("%d change(s) applied", e.changes)
	}
	head, _ := git(e.Repo, "log", "-1", "--format=%h")
	fmt.Fprintln(e.Out)
	e.box(sDone, "✓ "+msg, "host "+host+" · "+head)
	return nil
}

// hostID is chosen once and stored: hostname is unstable on macOS.
func (e *Env) hostID() (string, error) {
	p := filepath.Join(e.Cfg, "host")
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b)), nil
	}
	def, _ := os.Hostname()
	def, _, _ = strings.Cut(def, ".")
	id := e.Input("Host id for this machine (names hosts/<id>.json)", def)
	if id == "" {
		return "", errors.New("empty host id")
	}
	if err := os.MkdirAll(e.Cfg, 0o755); err != nil {
		return "", err
	}
	return id, os.WriteFile(p, []byte(id+"\n"), 0o644)
}

func (e *Env) syncRepo() error {
	e.step("Repo")
	st, err := git(e.Repo, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("%s is not a usable git repo: %s", e.Repo, st)
	}
	if st != "" {
		fmt.Fprintln(e.Out, st)
		e.warn("uncommitted changes in the repo (a capture or merge?): commit them, e.g. git -C %s add -A && git -C %s commit, then re-run", e.short(e.Repo), e.short(e.Repo))
		return errStop
	}
	if _, err := git(e.Repo, "rev-parse", "-q", "--verify", "@{u}"); err != nil {
		e.info("no upstream, skipping pull")
		return nil
	}
	err = e.Spin("pulling…", func() error {
		if out, err := git(e.Repo, "pull", "--ff-only", "-q"); err != nil {
			return fmt.Errorf("git pull: %s", out)
		}
		return nil
	})
	if err != nil {
		return err
	}
	head, _ := git(e.Repo, "log", "-1", "--format=%h %s")
	e.ok("at %s", head)
	return nil
}

// --- settings -------------------------------------------------------------

type obj = map[string]any

func readObj(p string) (obj, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	o := obj{}
	if err := d.Decode(&o); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return o, nil
}

// canon is sorted-key, indented JSON without HTML escaping (hooks contain &&).
func canon(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return b.Bytes()
}

func writeObj(p string, o obj) error { return os.WriteFile(p, canon(o), 0o644) }

func pickKeys(o obj, keys []string) obj {
	r := obj{}
	for _, k := range keys {
		if v, ok := o[k]; ok {
			r[k] = v
		}
	}
	return r
}

func (e *Env) syncSettings(host string) (done bool, err error) {
	e.step("Settings")
	live := filepath.Join(e.Claude, "settings.json")
	gen := filepath.Join(e.Claude, ".settings.generated")
	base := filepath.Join(e.Repo, "settings.base.json")
	h := filepath.Join(e.Repo, "hosts", host+".json")
	hl := filepath.Join(e.Repo, "hosts", host+".local.json")

	if err := os.MkdirAll(e.Claude, 0o755); err != nil { // fresh machine: claude never ran
		return false, err
	}
	liveObj, err := readObj(live)
	if errors.Is(err, fs.ErrNotExist) {
		liveObj, err = nil, nil
	}
	if err != nil {
		return false, err
	}

	if _, err := os.Stat(h); errors.Is(err, fs.ErrNotExist) {
		if !e.Confirm(fmt.Sprintf("No overlay for '%s'. Create hosts/%s.json from current settings?", host, host)) {
			e.warn("no hosts/%s.json and creating it was declined; stopped", host)
			return false, errStop
		}
		if err := os.MkdirAll(filepath.Dir(h), 0o755); err != nil {
			return false, err
		}
		if err := writeObj(h, pickKeys(liveObj, hostKeys)); err != nil {
			return false, err
		}
		if err := writeObj(hl, pickKeys(liveObj, localKeys)); err != nil {
			return false, err
		}
		e.ok("created hosts/%s.json (+ .local.json, gitignored)", host)
	}
	if _, err := os.Stat(hl); errors.Is(err, fs.ErrNotExist) {
		if err := writeObj(hl, obj{}); err != nil {
			return false, err
		}
	}

	// base + host + host.local; a top-level key replaces, no deep merge.
	merged := obj{}
	for _, p := range []string{base, h, hl} {
		o, err := readObj(p)
		if err != nil {
			return false, err
		}
		for k, v := range o {
			merged[k] = v
		}
	}

	// What live should look like if nothing changed it since the last sync.
	expect := merged
	if g, err := readObj(gen); err == nil {
		expect = g
	}

	if liveObj != nil && !bytes.Equal(canon(liveObj), canon(expect)) {
		e.warn("%s changed since the last sync:", e.short(live))
		e.showDiff(canon(expect), canon(liveObj))
		switch e.pick("settings.json drifted", "abort", "capture", "overwrite") {
		case "capture":
			if err := e.captureSettings(liveObj, expect, base, h, hl); err != nil {
				return false, err
			}
			if err := writeObj(gen, liveObj); err != nil {
				return false, err
			}
			e.warn("captured into the repo: review, commit, push, then re-run")
			return true, nil
		case "overwrite":
			if err := e.backup(live); err != nil {
				return false, err
			}
			liveObj = nil
		default:
			e.warn("aborted, nothing written")
			return false, errStop
		}
	}

	if liveObj != nil && bytes.Equal(canon(liveObj), canon(merged)) {
		e.ok("settings.json up to date")
	} else {
		if err := writeObj(live, merged); err != nil {
			return false, err
		}
		e.changes++
		e.ok("settings.json written")
	}
	return false, writeObj(gen, merged)
}

// captureSettings writes each changed top-level key back to the file that owns it.
func (e *Env) captureSettings(live, expect obj, base, h, hl string) error {
	keys := map[string]bool{}
	for k := range live {
		keys[k] = true
	}
	for k := range expect {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	hostObj, err := readObj(h)
	if err != nil {
		return err
	}
	for _, k := range sorted {
		lv, inLive := live[k]
		ev, inExp := expect[k]
		if inLive == inExp && bytes.Equal(canon(lv), canon(ev)) {
			continue
		}
		f := base
		if _, inHost := hostObj[k]; slices.Contains(localKeys, k) {
			f = hl
		} else if inHost || slices.Contains(hostKeys, k) {
			f = h
		}
		o, err := readObj(f)
		if err != nil {
			return err
		}
		if inLive {
			o[k] = lv
		} else {
			delete(o, k)
		}
		if err := writeObj(f, o); err != nil {
			return err
		}
		e.ok("captured '%s' → %s", k, e.rel(f))
	}
	return nil
}

// --- links ----------------------------------------------------------------

func (e *Env) syncLinks() error {
	e.step("Links")
	names := []string{"CLAUDE.md", "statusline.sh"}
	if ok, _ := e.have("rtk"); ok {
		names = append(names, "RTK.md")
	} else {
		e.info("rtk not installed, RTK.md not linked")
	}
	for _, sub := range []string{"commands", "skills"} {
		ents, _ := os.ReadDir(filepath.Join(e.Repo, sub))
		for _, d := range ents {
			if (sub == "commands" && strings.HasSuffix(d.Name(), ".md")) || (sub == "skills" && d.IsDir()) {
				names = append(names, filepath.Join(sub, d.Name()))
			}
		}
	}
	for _, n := range names {
		if err := e.link(filepath.Join(e.Repo, n), filepath.Join(e.Claude, n)); err != nil {
			return err
		}
	}
	return nil
}

func (e *Env) link(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return nil // optional file missing from the repo
	}
	fi, err := os.Lstat(dst)
	if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if t, _ := os.Readlink(dst); t == src {
			e.info("%s", e.short(dst))
			return nil
		}
	}
	if err == nil && fi.Mode().IsRegular() {
		a, _ := os.ReadFile(src)
		b, _ := os.ReadFile(dst)
		if !bytes.Equal(a, b) {
			// A real file where our link should be: maybe an atomic write replaced it.
			e.warn("%s is a real file that differs from the repo:", e.short(dst))
			e.showDiff(a, b)
			switch e.pick(filepath.Base(dst)+" drifted", "skip", "capture", "replace") {
			case "capture":
				if err := os.WriteFile(src, b, fi.Mode().Perm()); err != nil {
					return err
				}
				e.ok("captured → %s", e.rel(src))
			case "replace":
			default:
				e.warn("skipped")
				return nil
			}
		}
	}
	if err == nil {
		if err := e.backup(dst); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(src, dst); err != nil {
		return err
	}
	e.changes++
	e.ok("linked %s", e.short(dst))
	return nil
}

// --- agent skills ---------------------------------------------------------

// symlinkedSkills are top-level entries of ~/.agents/skills that are links
// (e.g. to a project checkout); they are neither copied nor deleted.
func symlinkedSkills(dir string) map[string]bool {
	skip := map[string]bool{}
	ents, _ := os.ReadDir(dir)
	for _, d := range ents {
		if d.Type()&fs.ModeSymlink != 0 {
			skip[d.Name()] = true
		}
	}
	return skip
}

// manifest is a sorted "sha1  path" list of real skill files plus the lock,
// the same lines `shasum` prints, so a manifest written by sync.sh still compares.
func (e *Env) manifest() []string {
	root := filepath.Join(e.Agents, "skills")
	var lines []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		r, _ := filepath.Rel(root, p)
		sum := sha1.Sum(b)
		lines = append(lines, hex.EncodeToString(sum[:])+"  ./"+r)
		return nil
	})
	if b, err := os.ReadFile(filepath.Join(e.Agents, ".skill-lock.json")); err == nil {
		sum := sha1.Sum(b)
		lines = append(lines, hex.EncodeToString(sum[:])+"  .skill-lock.json")
	}
	sort.Strings(lines)
	return lines
}

func readLines(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var l []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if t := strings.TrimSpace(s.Text()); t != "" {
			l = append(l, t)
		}
	}
	sort.Strings(l)
	return l, s.Err()
}

func (e *Env) syncAgents() (done bool, err error) {
	e.step("Agent skills")
	live := filepath.Join(e.Agents, "skills")
	repo := filepath.Join(e.Repo, "agents", "skills")
	lockLive := filepath.Join(e.Agents, ".skill-lock.json")
	lockRepo := filepath.Join(e.Repo, "agents", ".skill-lock.json")
	agen := filepath.Join(e.Claude, ".agents.generated")
	if err := os.MkdirAll(live, 0o755); err != nil {
		return false, err
	}
	skip := symlinkedSkills(live)
	saveManifest := func() error {
		return os.WriteFile(agen, []byte(strings.Join(e.manifest(), "\n")+"\n"), 0o644)
	}

	if prev, err := readLines(agen); err == nil {
		if cur := e.manifest(); !slices.Equal(prev, cur) {
			e.warn("%s changed since the last sync (npx skills add/update?):", e.short(e.Agents))
			e.manifestDiff(prev, cur)
			switch e.pick("~/.agents drifted", "abort", "capture", "overwrite") {
			case "capture":
				if _, err := mirror(live, repo, skip); err != nil {
					return false, err
				}
				if _, err := copyFile(lockLive, lockRepo); err != nil {
					return false, err
				}
				e.warn("captured into the repo: review, commit, push, then re-run")
				return true, saveManifest()
			case "overwrite":
			default:
				e.warn("aborted")
				return false, errStop
			}
		}
	} else if added, differ := localSkills(live, repo, skip); len(added)+len(differ) > 0 {
		// Never synced here: the mirror below would delete this machine's own skills.
		e.warn("first sync on this machine: %s has skills the repo does not:", e.short(live))
		if len(added) > 0 {
			fmt.Fprintln(e.Out, sOK.Render("    only here: "+strings.Join(added, ", ")))
		}
		if len(differ) > 0 {
			fmt.Fprintln(e.Out, sWarn.Render("    different from the repo: "+strings.Join(differ, ", ")))
		}
		switch e.pick("keep this machine's skills?", "abort", "merge", "overwrite") {
		case "merge", "capture":
			// Only skills the repo lacks are copied in; for a differing one the
			// repo copy wins and the local one is backed up below by the mirror step.
			if err := mergeLock(lockLive, lockRepo); err != nil {
				return false, err
			}
			for _, n := range added {
				if err := copyEntry(filepath.Join(live, n), filepath.Join(repo, n)); err != nil {
					return false, err
				}
				e.ok("merged agents/skills/%s", n)
			}
			for _, n := range differ {
				if err := e.backup(filepath.Join(live, n)); err != nil {
					return false, err
				}
				e.warn("%s: kept the repo version; this machine's copy is in the backup", n)
			}
			e.warn("merged into the repo, nothing deleted: commit and push it, then re-run")
			return true, nil
		case "overwrite":
			for _, n := range append(added, differ...) {
				if err := e.backup(filepath.Join(live, n)); err != nil {
					return false, err
				}
			}
			if _, err := os.Stat(lockLive); err == nil {
				if _, err := copyFile(lockLive, filepath.Join(e.backupDirOrNew(), ".agents", ".skill-lock.json")); err != nil {
					return false, err
				}
			}
		default:
			e.warn("aborted, nothing changed in %s", e.short(e.Agents))
			return false, errStop
		}
	}

	changed, err := mirror(repo, live, skip)
	if err != nil {
		return false, err
	}
	lockChanged, err := copyFile(lockRepo, lockLive)
	if err != nil {
		return false, err
	}
	if changed || lockChanged {
		e.changes++
		e.ok("~/.agents/skills updated")
	} else {
		e.ok("~/.agents/skills up to date")
	}
	if err := saveManifest(); err != nil {
		return false, err
	}

	// Claude only sees these through ~/.claude/skills/<name> links (what npx skills creates).
	ents, _ := os.ReadDir(repo)
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		dst := filepath.Join(e.Claude, "skills", d.Name())
		if fi, err := os.Lstat(dst); err == nil {
			if fi.Mode()&fs.ModeSymlink == 0 {
				e.warn("%s exists and is not a link, left alone", e.short(dst))
			}
			continue
		}
		target, _ := filepath.Rel(filepath.Dir(dst), filepath.Join(live, d.Name()))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return false, err
		}
		if err := os.Symlink(target, dst); err != nil {
			return false, err
		}
		e.changes++
		e.ok("linked skill %s", d.Name())
	}
	return false, nil
}

// localSkills: top-level entries of live (dirs or stray files, not the
// skipped symlinks) the repo lacks (added) or holds differently (differ).
func localSkills(live, repo string, skip map[string]bool) (added, differ []string) {
	ents, _ := os.ReadDir(live)
	for _, d := range ents {
		if skip[d.Name()] {
			continue
		}
		r := dirSum(filepath.Join(repo, d.Name()))
		switch {
		case r == "":
			added = append(added, d.Name())
		case r != dirSum(filepath.Join(live, d.Name())):
			differ = append(differ, d.Name())
		}
	}
	return added, differ
}

// dirSum hashes paths, modes, file contents and link targets under p (a dir
// or a file), skipping .git; "" when p is missing. Unreadable entries hash
// to a marker, so they count as different rather than equal.
func dirSum(p string) string {
	if _, err := os.Lstat(p); err != nil {
		return ""
	}
	h := sha1.New()
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		r, _ := filepath.Rel(p, q)
		if err != nil {
			fmt.Fprintf(h, "%s\x00unreadable\x00", r)
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			fmt.Fprintf(h, "%s\x00unreadable\x00", r)
			return nil
		}
		fmt.Fprintf(h, "%s\x00%v\x00", r, info.Mode())
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(q)
			fmt.Fprintf(h, "%s\x00", t)
		case d.Type().IsRegular():
			b, err := os.ReadFile(q)
			if err != nil {
				fmt.Fprintf(h, "unreadable\x00")
				return nil
			}
			fmt.Fprintf(h, "%x\x00", sha1.Sum(b))
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// copyEntry copies a skill dir (without .git, which git would record as an
// embedded repo and never ship) or a single file into the repo.
func copyEntry(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		_, err := copyFile(src, dst)
		return err
	}
	_, err = mirror(src, dst, map[string]bool{".git": true})
	return err
}

func (e *Env) backupDirOrNew() string {
	if e.backupDir == "" {
		e.backupDir = filepath.Join(e.Claude, "backups", "sync-"+time.Now().Format("20060102-150405"))
	}
	return e.backupDir
}

// mergeLock adds live lock entries the repo lock lacks; repo entries win.
func mergeLock(live, repo string) error {
	l, err := readObj(live)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := readObj(repo)
	if errors.Is(err, fs.ErrNotExist) {
		_, err = copyFile(live, repo)
		return err
	}
	if err != nil {
		return err
	}
	ls, _ := l["skills"].(map[string]any)
	rs, _ := r["skills"].(map[string]any)
	if rs == nil {
		rs = map[string]any{}
	}
	for k, v := range ls {
		if _, ok := rs[k]; !ok {
			rs[k] = v
		}
	}
	r["skills"] = rs
	return writeObj(repo, r)
}

// syncClaudeSkills offers to capture skills made directly in ~/.claude/skills
// (real dirs with a SKILL.md that neither repo skill dir has).
func (e *Env) syncClaudeSkills() (done bool, err error) {
	dir := filepath.Join(e.Claude, "skills")
	var found []string
	ents, _ := os.ReadDir(dir)
	for _, d := range ents {
		n := d.Name()
		if d.Type()&fs.ModeSymlink != 0 || !d.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, n, "SKILL.md")); err != nil {
			continue // e.g. Claude's own learned/ and synced/ stores
		}
		if dirSum(filepath.Join(e.Repo, "skills", n)) != "" || dirSum(filepath.Join(e.Repo, "agents", "skills", n)) != "" {
			continue
		}
		found = append(found, n)
	}
	if len(found) == 0 {
		return false, nil
	}
	e.warn("skills only on this machine, in %s: %s", e.short(dir), strings.Join(found, ", "))
	if e.pick("add them to the repo?", "skip", "capture") != "capture" {
		e.info("left alone; `gh ccsync capture` adds them")
		return false, nil
	}
	for _, n := range found {
		if err := copyEntry(filepath.Join(dir, n), filepath.Join(e.Repo, "skills", n)); err != nil {
			return false, err
		}
		e.ok("captured skills/%s", n)
	}
	e.warn("captured into the repo: commit and push it, then re-run")
	return true, nil
}

// manifestDiff prints up to 20 added (+), changed (~) and removed (-) paths.
func (e *Env) manifestDiff(prev, cur []string) {
	index := func(ls []string) map[string]string {
		m := map[string]string{}
		for _, l := range ls {
			_, p, _ := strings.Cut(l, "  ")
			m[p] = l
		}
		return m
	}
	pm, cm := index(prev), index(cur)
	var out []string
	for p, l := range cm {
		if old, ok := pm[p]; !ok {
			out = append(out, sOK.Render("    + "+p))
		} else if old != l {
			out = append(out, sWarn.Render("    ~ "+p))
		}
	}
	for p := range pm {
		if _, ok := cm[p]; !ok {
			out = append(out, sWarn.Render("    - "+p))
		}
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = append(out[:20], sInfo.Render(fmt.Sprintf("    … %d more", len(out)-20)))
	}
	fmt.Fprintln(e.Out, strings.Join(out, "\n"))
}

// mirror makes dst match src (like rsync -a --delete) and reports whether
// anything changed. Top-level names in skip are left alone on both sides.
func mirror(src, dst string, skip map[string]bool) (bool, error) {
	changed := false
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return false, err
	}
	srcEnts, err := os.ReadDir(src)
	if err != nil {
		return false, err
	}
	want := map[string]bool{}
	for _, d := range srcEnts {
		want[d.Name()] = true
	}
	dstEnts, _ := os.ReadDir(dst)
	for _, d := range dstEnts {
		if !want[d.Name()] && !skip[d.Name()] {
			if err := os.RemoveAll(filepath.Join(dst, d.Name())); err != nil {
				return false, err
			}
			changed = true
		}
	}
	for _, d := range srcEnts {
		if skip[d.Name()] {
			continue
		}
		s, t := filepath.Join(src, d.Name()), filepath.Join(dst, d.Name())
		var c bool
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(s)
			if err != nil {
				return false, err
			}
			if cur, err := os.Readlink(t); err == nil && cur == target {
				continue
			}
			_ = os.RemoveAll(t)
			if err := os.Symlink(target, t); err != nil {
				return false, err
			}
			c = true
		case d.IsDir():
			if fi, err := os.Lstat(t); err == nil && !fi.IsDir() {
				_ = os.RemoveAll(t)
			}
			if c, err = mirror(s, t, nil); err != nil {
				return false, err
			}
		default:
			if fi, err := os.Lstat(t); err == nil && (fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0) {
				_ = os.RemoveAll(t)
			}
			if c, err = copyFile(s, t); err != nil {
				return false, err
			}
		}
		changed = changed || c
	}
	return changed, nil
}

// copyFile writes src to dst when content or mode differ. A missing src is not an error.
func copyFile(src, dst string) (bool, error) {
	b, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	si, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	mode := si.Mode().Perm()
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, b) {
		if di, err := os.Stat(dst); err == nil && di.Mode().Perm() == mode {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		return false, err
	}
	return true, os.Chmod(dst, mode)
}
