# wharfy

Ship one binary to every channel — a release/distribution CLI built for **AI agents to drive**.

[![ci](https://github.com/ShiroDoromoto/wharfy/actions/workflows/ci.yml/badge.svg)](https://github.com/ShiroDoromoto/wharfy/actions/workflows/ci.yml)

You declare what to build; wharfy cross-compiles it and ships it to Homebrew, Scoop,
apt/rpm, containers, AUR, winget, `go install`, and a `curl | sh` installer — wrapping
[GoReleaser](https://goreleaser.com/) and adding a self-describing, machine-readable surface.

## Why

Getting your tool installable "the usual way" on every platform is fiddly: each channel has
its own format (formula / manifest / winget YAML / deb·rpm + repo metadata), plus signing,
tags, and publishing. Existing tools assume **a human reads docs and writes YAML**. Point an
AI agent at that and most of the budget burns *before* the actual release — discovering
commands, parsing unstructured output, re-deriving "what's published where", guarding
destructive steps.

wharfy closes that by speaking through its output. Three rules hold everywhere:

1. **Self-describing** — `wharfy agent` returns the whole capability map (commands, args,
   order, channels, where to read state) in one shot. Read once, then drive.
2. **Next step, always** — every command takes `--json` and ends with a `next:` block whose
   `do` lines are runnable commands.
3. **Non-destructive** — wharfy only writes the *distribution artifacts it owns* (your tap,
   bucket, release assets, …). It never touches your source or CI — it shows a diff and asks.

## Install

```sh
# Homebrew
brew install ShiroDoromoto/wharfy/wharfy

# go install
go install github.com/ShiroDoromoto/wharfy/cmd/wharfy@latest

# curl | sh
curl -fsSL https://github.com/ShiroDoromoto/wharfy/releases/latest/download/install.sh | sh
```

(wharfy is shipped by wharfy — all three are produced by the channels below.)

## Quick start

A minimal `wharfy.yaml` (most of it is inferred from your git remote and conventions):

```yaml
channels: [homebrew, releases, script, goinstall]
# project / github / main / homepage / license are inferred when possible.
# add owned channels that need infra explicitly: scoop, apt, rpm, container, aur, winget
```

Once, so future agents don't reinvent your release: `wharfy init --yes` writes a small managed
block to `AGENTS.md` and `CLAUDE.md` telling agents to run `wharfy agent` instead of guessing
release steps. Without `--yes` it previews; on a file you already have, it appends one block
(re-running just refreshes it — idempotent).

Then drive — start by asking the tool what it can do:

```sh
wharfy agent                 # one-screen capability map (use --json from an agent)
wharfy config                # the resolved effective config
wharfy build                 # cross-compile (via GoReleaser) → artifacts
wharfy release --yes         # upload the github release (archives, packages, install.sh)
wharfy publish homebrew --dry-run   # preview the formula diff before writing
wharfy publish --yes         # write each channel's manifest (reuses the release)
wharfy status                # what's built / released / published / drifted, and the next move
```

The order is `build → sign → release → publish → verify` (what `wharfy agent` reports).
`release` uploads the GitHub release once and records the artifacts; `publish` then writes
each channel's manifest against that release, so a mid-batch failure resumes safely without
re-uploading. `wharfy publish` with no prior `release` still works — it runs the release itself.

Every command also takes `--json` and ends with a `next:` block. **The authoritative,
always-current list of commands and channels is `wharfy agent` itself** — this README does
not duplicate it (a generated map can't go stale; a hand-written table can).

## Channels

Owned (wharfy publishes directly): `homebrew`, `scoop`, `apt`, `rpm`, `container` (ghcr,
multi-arch), `aur`, `script` (`curl | sh`), `goinstall`.
Gated (wharfy prepares a PR and tracks it, never merges): `winget`, `homebrew-core`.

The GitHub Release itself (archives, deb/rpm, `install.sh`) is produced by `release`, not
`publish` — direct download and `curl | sh` install come from there, and the owned channels
above reuse it. (`wharfy publish` only accepts the channels listed here.)

Run `wharfy agent --json` for the live set and each channel's kind.

Each channel needs its own prerequisites (a token, a hosted repo, docker, an AUR key, …).
wharfy tells you what's missing: `publish --dry-run` lists a `requires` block, and unconfigured
channels are skipped (not failed) in a batch. Owned tap/bucket repos are created for you on
`--yes` (a `tap_will_be_created` warning previews it).

`apt`/`rpm` need a hosted package repo (a deb/rpm server is more than a git repo: it serves
index metadata, and `apt`/`rpm` upload and serve from different hosts). Set it in `wharfy.yaml`
the low-friction way — a managed service via `provider`, where one user namespace yields both
the upload and delivery URLs:

```yaml
apt: { provider: fury, user: <name> }   # delivery: https://apt.fury.io/<name>/, upload: push.fury.io
rpm: { provider: fury, user: <name> }   # delivery: https://yum.fury.io/<name>/
```

Or give raw URLs for any host — `{ repo: <delivery-url>, push: <upload-url> }` (omit `push` when
upload and delivery share a host). When `repo` is unset, `publish` skips the channel and its
`next:` block walks you through the hosting options.

The upload token is **never written to `wharfy.yaml` or generated files**. Pass it via the
`PACKAGE_REPO_TOKEN` environment variable (good for CI), or save it once to your OS keychain with
`wharfy auth fury` — it prompts hidden (the value never reaches your shell history or an agent's
transcript) and `publish` then loads it from the keychain when the env var is unset.

### Runtime dependencies

If your binary shells out to another tool at runtime, declare it so "the usual install" pulls
it in too. Each owned package channel emits it in that channel's native form:

```yaml
homebrew: { dependencies: [git] }                       # → depends_on "git"
scoop:    { dependencies: [git] }                       # → manifest "depends"
apt:      { provider: fury, user: <name>, depends: [git], recommends: [bash-completion] }
rpm:      { provider: fury, user: <name>, depends: [git-core] }   # package names differ per distro
```

`apt`/`rpm` keep `depends` (required) / `recommends` / `suggests` separate — deb's three tiers
(rpm maps them to `Requires` + weak deps) — and each set is scoped to its own format, so the
package names can differ across distros. Output is deterministic (sorted); omit the key and the
generated artifact is unchanged. (`homebrew-core` source-build formulae also get these as
`depends_on`, alongside the build-only `go`.)

Gated channels also have *external* acceptance criteria that wharfy can't satisfy for you, and
some are **strict**. `homebrew-core` requires a notable, established project **and** a formula
that passes `brew audit --new --strict`. For it, wharfy generates a **source-build** formula
(`go build` from the tagged source, not a prebuilt binary — that's the core-appropriate shape;
your own tap stays binary), surfaces the acceptance criteria up front, and **refuses to open a
PR unless you pass `--acknowledge-review`** (so casual runs don't burden Homebrew maintainers
with doomed PRs). The generated formula is a starting point, not a guaranteed audit pass; run
`brew audit --new --strict` yourself first. `winget`, by contrast, is a broad self-service
index and stays low-friction.

## How it works

- **Wraps GoReleaser** as a pinned subprocess for cross-build, archives, nfpm packages, and
  container images. The boundary is a `Builder` interface, so the engine is swappable.
- **Owns the distribution artifacts** and writes them under `.wharfy/` (never your repo root)
  or to the channel target (tap/bucket/release/registry). Secrets come from env only.
- **Hybrid state** (`.wharfy/state.json` + live probes): `status` reconciles your record
  against reality and surfaces *drift* instead of silently "fixing" it.
- Output is one `Result` envelope rendered as human text or `--json` against the schemas in
  [`schemas/`](schemas/).

## Maturity

MVP. wharfy ships **itself** end-to-end via Homebrew (the strongest dogfood). The other
channels are implemented and unit-tested but are first-run against real infrastructure. Expect
rough edges on channels you haven't exercised; `--dry-run` first.

## Development

```sh
go test ./...     # unit + drift + schema-validation tests (no goreleaser/network needed)
go vet ./...
```

CI runs gofmt / vet / build / `go test -race` on every push and PR.

## License

[AGPL-3.0](LICENSE)
