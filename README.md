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

Then drive — start by asking the tool what it can do:

```sh
wharfy agent                 # one-screen capability map (use --json from an agent)
wharfy config                # the resolved effective config
wharfy build                 # cross-compile (via GoReleaser) → artifacts
wharfy publish homebrew --dry-run   # preview the formula diff before writing
wharfy publish --yes         # publish all configured channels (one release)
wharfy status                # what's built / published / drifted, and the next move
```

Every command also takes `--json` and ends with a `next:` block. **The authoritative,
always-current list of commands and channels is `wharfy agent` itself** — this README does
not duplicate it (a generated map can't go stale; a hand-written table can).

## Channels

Owned (wharfy publishes directly): `homebrew`, `scoop`, `apt`, `rpm`, `container` (ghcr,
multi-arch), `aur`, `releases` (GitHub Releases), `script` (`curl | sh`), `goinstall`.
Gated (wharfy prepares a PR and tracks it, never merges): `winget`.

Run `wharfy agent --json` for the live set and each channel's kind.

Each channel needs its own prerequisites (a tap/bucket repo, a token, a hosted repo, docker,
an AUR key, …). wharfy tells you what's missing: `publish --dry-run` lists a `requires` block,
and unconfigured channels are skipped (not failed) in a batch.

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
channels are implemented and unit-tested but are first-run against real infrastructure — and
wharfy does not yet auto-create owned tap/bucket repos (create them first). Expect rough edges
on channels you haven't exercised; `--dry-run` first.

## Development

```sh
go test ./...     # unit + drift + schema-validation tests (no goreleaser/network needed)
go vet ./...
```

CI runs gofmt / vet / build / `go test -race` on every push and PR.

## License

[AGPL-3.0](LICENSE)
