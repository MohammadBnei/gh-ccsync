# 1. First-sync settings merge goes to the host overlay

- Status: Accepted
- Date: 2026-10-06

## Context

On a machine's first sync (seen on WSL), `~/.claude/settings.json` already
exists but `~/.claude/.settings.generated` does not. The sync compared live
settings against base + `hosts/<id>.json` + `hosts/<id>.local.json` and reported
everything that differed as drift. Every answer was wrong:

- `capture` routed non-host keys into the shared `settings.base.json`, which
  then overrode every other machine on its next pull.
- `overwrite` discarded the machine's own settings (backed up, but gone).

The differences come both as whole keys (`theme`, `model`) and as nested values
inside a shared key (`permissions.allow`, `hooks`).

## Decision

1. On a first sync, drift is only the keys live has that the merged result
   lacks or holds differently. Keys only the repo has are written without
   asking.
2. `merge` (also `capture` and `CCSYNC_CHOICE=merge`) writes live's value of each
   drifted key into `hosts/<id>.json`, or `hosts/<id>.local.json` for local keys.
   It never writes the shared base and never deletes a key.
3. When that changed `hosts/<id>.json`, and only on a tty with `claude`
   installed, ccsync offers to open an interactive `claude` in the repo to move
   what every machine shares into `settings.base.json`. ccsync then stops; the
   user reviews with git and commits. It never runs `claude -p`, never runs it
   headless, and never commits.
4. Every sync prints the keys `hosts/<id>.json` overrides in base, because base
   edits to those keys no longer reach that machine.

Quality attribute priority: the safety of the shared base and deterministic
behavior win over automation. git stays the only runtime dependency.

## Alternatives considered

- **Claude as the default resolver.** Rejected: on a first sync `claude` is often
  not installed or not logged in yet, and every sync would spend tokens.
- **Claude only.** Rejected: breaks headless runs and fresh machines.
- **`claude -p` with a diff gate.** Rejected: abort cannot restore the gitignored
  `.local.json` or a just-created untracked `hosts/<id>.json`; `acceptEdits`
  cannot be limited to the three settings files; and claude would load the very
  hooks and plugins being reconciled.
- **Deep merge (recurse objects, union arrays).** Rejected: changes layering for
  every machine, and whole-key routing to the overlay is already correct, only
  redundant.
- **Auto-apply a resolver's output.** Rejected: a bad base edit spreads to every
  machine on the next pull.
- **Keep `capture` into base.** This was the bug.

## Consequences

- Host overlays may duplicate or shadow base keys. The shadow notice makes this
  visible; cleaning it up is manual or done through the claude handoff.
- Later captures of a key already in `hosts/<id>.json` keep going there.

## Out of scope

Claude resolution for `~/.agents` and link drift, deep merge, auto-commit.
