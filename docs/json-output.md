# MonoFlow `--json` output and repo settings

## Versioned JSON (`"schema"`)

Every JSON object MonoFlow prints with `--json` starts with `"schema": 1`
(`cmd/monoflow/textout.go`, `JSONSchemaVersion`). The contract:

- Within one schema version, fields are only **added**, never renamed or removed.
- A rename or removal bumps the version; the old field is still printed for one more version.
  Example: `graph impact` printed `owning_package` before it became `package`; schema 1 prints both.
- Lists (`checkpoint list`, the bare `graph`) are printed as they are.
- Unknown or unsupported flags fail with exit code 2 (`"code": "unknown_flag"` in JSON) instead of
  being ignored.

The shapes of the main commands are in [`schema/monoflow-output.schema.json`](schema/monoflow-output.schema.json).

## Settings in `.agents/monoflow.config.json`

Besides `packages` and `verify_exclude`:

| key | effect |
| --- | --- |
| `global_inputs` | Patterns of files that affect **every** package (added to the built-in list: lockfiles, `go.work`, `rust-toolchain.toml`, `.github/workflows/**`, ...). |
| `ignore_inputs` | Patterns of files that affect nothing, on purpose (`docs/**`, `*.md`). |
| `unmapped` | `"nothing"` (default) or `"all"`: what a changed file no package owns affects. Unowned files are listed as `unmapped` by `verify` and `graph affected` either way. |
| `shared_worktree` | `true` when several agents edit this checkout at once: `checkpoint revert --force`, `ship` moving the tree, `outer start --revert` and `sync` with uncommitted work (autostash) refuse, and background speculation is off. `MONOFLOW_SHARED_WORKTREE=1` does the same. |
| `speculate` | `true` / `false` forces background verify on or off. Unset: on, except in a shared worktree or when more than one agent session hooked into this checkout in the last 15 minutes. |

Patterns: `dir/**` is everything under `dir`, `**/x` is `x` in any directory, a wildcard without a slash
(`*.md`) matches the base name anywhere, and a plain path (`Cargo.toml`) is that path from the root only.

Packages that share a name (an npm workspace and a Cargo crate both called `wisp`) are all kept, each
addressed as `name@path` (`wisp@crates/wisp`, `wisp@root`). `verify` and `graph` warn about it on stderr.
