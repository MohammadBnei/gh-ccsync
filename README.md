# gh-ccsync

A gh extension that syncs a Claude Code config repo (e.g. a private
`claude-config`) into `~/.claude` and `~/.agents`. It's a single static binary,
and git is its only runtime dependency.

    gh extension install MohammadBnei/gh-ccsync
    gh ccsync init MohammadBnei/claude-config   # clone + first apply
    gh ccsync                                   # pull + apply
    gh ccsync capture                           # local changes → repo
    gh extension upgrade ccsync

## What a sync does

1. Refuses to run if the config repo has uncommitted edits, then `git pull --ff-only`.
2. Builds `settings.json` from `settings.base.json` + `hosts/<id>.json` +
   `hosts/<id>.local.json`. A top-level key replaces the one before it, with no
   deep merge. The host id is asked once and stored in `~/.config/claude-config/host`.
3. Symlinks `CLAUDE.md`, `statusline.sh`, `RTK.md` (only when rtk is installed),
   `commands/*.md` and `skills/*` into `~/.claude`.
4. Mirrors `agents/skills` into `~/.agents/skills` and links each skill into
   `~/.claude/skills`. Symlinked skills (local checkouts) are left alone.

If something changed locally since the last sync (Claude rewrote
settings.json, `npx skills add`, an edited CLAUDE.md), it shows the diff and asks
whether to abort, capture or overwrite. Every file it replaces is backed up to
`~/.claude/backups/sync-<timestamp>/`.

## Release

Push a `v*` tag. `cli/gh-extension-precompile` builds the binaries that
`gh extension install` picks from.
