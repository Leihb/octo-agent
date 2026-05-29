# Default Skills (embed + materialize)

> A curated set of skills ships with the binary and lands on disk on first run,
> so a fresh `octo` install has useful skills without the user authoring any.

## 1. Goal

After install, a curated skill set (today: `worktree-isolate`) is discoverable,
listable, and overridable — **offline, version-locked to the binary, and never
clobbering a user's own skills.**

## 2. Decision: embed, don't download

The default set is embedded in the binary via `//go:embed` and materialized to
disk on startup — **not** fetched from a remote. Embedding keeps a fresh install
offline-capable (works for `go install`, air-gapped, CI) and immune to
supply-chain / network-failure modes. A real download path is reserved for a
future *optional* community catalog (`octo skills add <name>`), which is a
separate concern from the built-in defaults.

## 3. Mechanism

- **Source of truth:** `internal/skills/defaults/<name>/SKILL.md`, embedded as
  `defaultsFS` (`internal/skills/defaults.go`).
- **Materialize:** `MaterializeDefaults(version)` writes the embedded tree to
  `~/.octo/skills-default/`, stamped with the binary version in `.octo-version`.
  It's a fast no-op once the stamp matches; a version bump wipes-and-rewrites the
  whole dir (stale/renamed skills handled). Called best-effort at startup in
  `cmd/octo` `run()` (skipped for the `__complete` / `__sandboxed-exec` fast
  paths); a read-only HOME degrades to "no defaults", never an error.
- **Dedicated dir:** `~/.octo/skills-default/` is exclusively octo-managed and
  kept **separate** from `~/.octo/skills/`, so refreshing defaults never touches
  a user's own skills and needs no per-file provenance tracking.

## 4. Precedence

`Discover` scans lowest-first; `scanRoot` overwrites by name:

```
default  ~/.octo/skills-default/   (shipped, materialized)
   ↓ overridden by
user     ~/.octo/skills/
   ↓ overridden by
project  ./.octo/skills/
```

A user overrides a default skill by dropping a same-named skill in
`~/.octo/skills/`. `--list-skills` / `octo skills list` tag each with its
source.

## 5. CLI

```
octo skills list      # all skills, grouped default → user → project
octo skills update    # force re-materialize the defaults (ignores the stamp)
octo skills path      # print the three roots
```

## 6. Adding a default skill

Drop `internal/skills/defaults/<name>/SKILL.md` (standard SKILL.md frontmatter:
`name` + `description`). It ships on the next release and materializes on the
user's next run after upgrade. Keep the set small and high-value — every default
costs system-prompt manifest space for every user.

## 7. Out of scope

- **Remote / community skills** (`octo skills add`, a catalog) — a separate
  optional feature; defaults stay embedded.
- **Per-file user-edit preservation in the default dir** — unnecessary, because
  users edit in `~/.octo/skills/`, not the octo-managed default dir.
