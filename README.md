# gh-ccsync

A gh extension that syncs a Claude Code config repo (e.g. a private
`claude-config`) into `~/.claude` and `~/.agents`. It's a single static binary,
and git is its only runtime dependency.

    gh extension install MohammadBnei/gh-ccsync
    gh ccsync init MohammadBnei/claude-config   # clone + first apply
    gh ccsync                                   # pull + apply
    gh ccsync capture                           # local changes → repo
    gh ccsync tools [--yes] [name…]             # install missing tools/plugins, update present ones
    gh extension upgrade ccsync

## What a sync does

1. Refuses to run if the config repo has uncommitted edits, then `git pull --ff-only`.
2. Checks the binaries in the repo's `tools.json` (see below) and offers to install
   the missing ones.
3. Builds `settings.json` from `settings.base.json` + `hosts/<id>.json` +
   `hosts/<id>.local.json`. A top-level key replaces the one before it, with no
   deep merge. The host id is asked once and stored in `~/.config/claude-config/host`.
4. Checks the plugins set to `true` in `enabledPlugins` and offers to install the
   missing ones (adding their marketplace from `extraKnownMarketplaces` first).
5. Symlinks `CLAUDE.md`, `statusline.sh`, `RTK.md` (only when rtk is installed),
   `commands/*.md` and `skills/*` into `~/.claude`.
6. Mirrors `agents/skills` into `~/.agents/skills` and links each skill into
   `~/.claude/skills`. Symlinked skills (local checkouts) are left alone.

On a machine's first sync, skills it already has that the repo lacks are
never deleted: it offers merge (copy them into the repo, union the skill lock,
delete nothing) / overwrite (with backup) / abort. Skills made directly in
`~/.claude/skills` (a real dir with a `SKILL.md`) are offered for capture into
`skills/` on every sync.

If something changed locally since the last sync (Claude rewrote
settings.json, `npx skills add`, an edited CLAUDE.md), it shows the diff and asks
whether to abort, capture or overwrite. Every file it replaces is backed up to
`~/.claude/backups/sync-<timestamp>/`.

## Tools

`tools.json` in the config repo lists the binaries the config depends on:

    {"rtk": {"install": {"darwin": "brew install rtk", "linux": "sh tools/x.sh"},
             "update": "…", "check": "rtk gain >/dev/null", "hint": "…"}}

- A command is a string (every OS) or a map keyed by GOOS. It runs with `sh -c`
  from the repo root, so it can call scripts kept in the repo.
- `$SUDO` is `sudo`, or empty when running as root. `DEBIAN_FRONTEND=noninteractive`
  is set, and `~/.local/bin` is on PATH.
- `check` is optional. When it fails, the binary counts as missing (e.g. the
  other `rtk`).
- A tool with no command for this OS prints its `hint` instead.

Nothing installs without consent:
- A plain sync lists the exact commands and asks: skip / install / never. "never"
  is remembered per machine in `~/.config/claude-config/tools.declined`.
- `gh ccsync tools` also runs the update commands. `--yes` is the only way to
  install without a terminal.
- `CCSYNC_CHOICE` never installs anything.
- When `tools.json` changed since you last approved it, the diff is shown first.

On Linux, installs need `curl`, `tar` and `sha256sum`. Tools that land in
`~/.local/bin` need a new login shell before `claude` sees them.

## Release

Merge to `main`, pull, then run `./release.sh vX.Y.Z` (it refuses anywhere else). It tests, cross-builds darwin/linux × amd64/arm64, and uploads them as
release assets named `*_<os>-<arch>`, which is what `gh extension install` picks from.
