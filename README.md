# wharfy

Ship one binary to every channel — a release/distribution CLI built for **AI agents to drive**.

[![ci](https://github.com/ShiroDoromoto/wharfy/actions/workflows/ci.yml/badge.svg)](https://github.com/ShiroDoromoto/wharfy/actions/workflows/ci.yml)

You declare what to build; wharfy cross-compiles it and ships it to Homebrew, Scoop,
apt/rpm, containers, AUR, winget, `go install`, and a `curl | sh` / PowerShell installer — wrapping
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

# curl | sh (macOS / Linux)
curl -fsSL https://github.com/ShiroDoromoto/wharfy/releases/latest/download/install.sh | sh

# PowerShell (Windows)
irm https://github.com/ShiroDoromoto/wharfy/releases/latest/download/install.ps1 | iex
```

(wharfy is shipped by wharfy — all of these are produced by the channels below. The
`install.sh` / `install.ps1` scripts need no package manager and install per-user, no sudo/admin.)

## Quick start

A minimal `wharfy.yaml` (most of it is inferred from your git remote and conventions):

```yaml
channels: [homebrew, releases, script, goinstall]
# project / github / main / homepage / license are inferred when possible.
# add owned channels that need infra explicitly: scoop, apt, rpm, container, aur, winget
```

Keys wharfy does not know are refused (`config_invalid`), naming the key and the line — a
misspelled key never runs on the defaults while you believe it took effect.

Once, so future agents don't reinvent your release: `wharfy init --yes` writes a small managed
block to `AGENTS.md` and `CLAUDE.md` telling agents to run `wharfy agent` instead of guessing
release steps. Without `--yes` it previews; on a file you already have, it appends one block
(re-running just refreshes it — idempotent).

Then drive — start by asking the tool what it can do:

```sh
wharfy agent                 # one-screen capability map (use --json from an agent)
wharfy config                # the resolved effective config
wharfy build                 # cross-compile (via GoReleaser) → artifacts
wharfy release --yes         # upload the github release (archives, packages, install.sh, latest.json)
wharfy publish homebrew --dry-run   # preview the formula diff before writing
wharfy publish --yes         # write each channel's manifest (reuses the release)
wharfy status                # what's built / released / published / drifted, and the next move
```

The order is `build → sign → release → publish → verify` (what `wharfy agent` reports).
`release` uploads the GitHub release once and records the artifacts; `publish` then writes
each channel's manifest against that release, so a mid-batch failure resumes safely without
re-uploading. `wharfy publish` with no prior `release` still works — it runs the release itself.

Both steps are re-runnable on the same tag: `release` reuses the existing GitHub release and
replaces the assets already on it, and `publish` skips the channels it completed. A workflow that
died halfway can just be run again — you never have to delete the release by hand first.

`verify` checks the channels in your `channels:` from the consumer's side — that list is what
decides the scope, not the publish history in `state.json` (a channel you dropped from the config
is never verified, and its old record no longer makes `verify` green).

It needs nothing local to run. Which version it checks comes from the publish record when there is
one, and otherwise from the channels themselves: the latest GitHub release, or failing that the
latest git tag. So a bare clone — a separate CI job, a scheduled workflow, your laptop a month
later — can still ask *does what we shipped still install?*, which is the question you actually want
to keep asking. `wharfy verify --version 0.4.1` names the version instead.

By default it installs nothing: it probes each channel over the network, so you can run it on every
CI build. For `homebrew` it reads the formula off the tap and matches the version; for `releases` it
checks that every asset listed in the release's own manifest (`latest.json`, plus the checksums file
`<project>_<version>_checksums.txt` when GoReleaser wrote one) really exists on the release — a user
following that manifest would otherwise hit a `404`. The binaries themselves are not downloaded, and
a release carrying neither manifest is reported `skipped`, not verified. For `script` it fetches both
published installers — `install.sh` and `install.ps1` — and matches the version each one installs; a
missing or stale `install.ps1` fails the check on any OS, so a Linux CI catches a Windows-only break
before your users do. For `goinstall` it asks the module proxy whether your tag resolves; for
`apt`/`rpm` it reads the version out of the repo metadata. Those five have more to check than a probe
can reach, so they are reported `partial`, never `verified`.

`wharfy verify --install` goes the rest of the way and installs for real. `apt`/`rpm` add your repo
inside a Debian/Fedora container, install the package and run it — a broken dependency or a wrong
file layout fails there, not on your users' machines. The upload itself can't catch that: it returns
`200` either way. `script` runs the installer your users on this host would run — `install.sh` into a
temporary `PREFIX`, or on Windows the published `install.ps1` into a temporary `WHARFY_PREFIX` — and
`goinstall` runs `go install` into a temporary `GOBIN`; both then run the binary they installed. Only
the installer for this host is run — the other one was already probed. Nothing lands on
your `PATH`. `releases` downloads every asset the checksums file lists and hashes it — an upload that
was cut short, or an asset swapped after the fact, keeps its name and passes the probe, so only the
`sha256` catches it. When the tool a channel needs is absent (no docker, no `sh` / `powershell`, no
`go`), or the base image cannot be pulled, that channel is reported `partial` with a warning — never
failed. So is a `releases` run with no checksums file to compare against. A `goinstall` binary is only
required to run, not to report the right version: `go install` does not apply your ldflags, so a CLI
that injects its version says `dev`.

If **no** channel could be verified at all, `verify` exits non-zero with `nothing_to_verify` rather
than reporting a green run it never made.

Two `--install` defaults can be moved. `verify.images` picks the base image per channel — verify where
you actually ship, not where wharfy guessed. `verify.run` replaces the launch check, which otherwise
guesses `--version`, then `version`, then `--help`; a CLI that requires a subcommand would fail all
three and be reported broken when it is not. Both apply wherever the binary is launched — in the
container for `apt`/`rpm`, on your machine for `script` and `goinstall`.

```yaml
verify:
  images:
    apt: ubuntu:24.04
    rpm: rockylinux:9
  run: [status, --quiet]   # runs `<binary> status --quiet` after installing
```

`wharfy verify apt` narrows the run to a single channel — useful while you fix one, since the
`--install` steps are slow. A name absent from `channels:` is refused with `channel_not_configured`,
exactly as `wharfy publish <channel>` refuses it.

Every command also takes `--json` and ends with a `next:` block. **The authoritative,
always-current list of commands and channels is `wharfy agent` itself** — this README does
not duplicate it (a generated map can't go stale; a hand-written table can).

## Channels

Owned (wharfy publishes directly): `homebrew`, `cask` (GUI apps), `scoop`, `apt`, `rpm`,
`container` (ghcr, multi-arch), `aur`, `script` (`curl | sh` + PowerShell `irm | iex`, per-user), `goinstall`.
Gated (wharfy prepares a PR and tracks it, never merges — and won't open a second PR while
an earlier one is still under review): `winget`, `homebrew-core`.

For a GUI app you ship built, signed bundles instead of compiling — declare `bundle:` (BYO-bundle,
the GUI counterpart of `prebuilt:`) with your `.dmg`/`.zip`/`.AppImage`/`.deb`/`.rpm`, and wharfy
uploads them to the Release and drives the GUI channels (defaults to `[cask, releases]`):

- **macOS** — Homebrew **Cask** to the *same tap* as your Formula (`cask` defaults to token
  `<project>-app`, tap `<owner>/homebrew-<project>`), so `wharfy status` lists CLI Formula and GUI
  Cask together.
- **Linux** — **AppImage** is a direct-download Release asset; `.deb`/`.rpm` go to the *same*
  hosted apt/rpm repo as the CLI under the bundler's own `<project>-app` package name.
- **Windows** — **Scoop** app manifest (`<project>-app`, portable zip with a Start-menu shortcut)
  and **winget** (gated PR, portable-zip installer).

`prebuilt:` (CLI) and `bundle:` (GUI) can be declared **together** — one `wharfy release` ships both
the CLI archives (and `install.sh`) and the GUI bundles to the same Release, and each channel reuses
its own artifacts.

For **bundles**, wharfy relays them as-is and never re-signs — a non-notarized macOS bundle gets a
Gatekeeper `caveats` note in the cask. For a **prebuilt CLI binary** (`prebuilt:`), signing is an
opt-in stage: set `sign: { identity: … }` (a keychain certificate name, or `WHARFY_SIGN_IDENTITY`;
for CI, a portable `.p12` via `WHARFY_SIGN_P12` + `WHARFY_SIGN_P12_PASSWORD`) and wharfy codesigns
the macOS Mach-O **before** cutting checksums. Secrets stay in env — never in `wharfy.yaml` or
generated files. Notarization is never required (self-signed completes).

**Bring your own pre-signed binaries.** Leave `sign.identity` unset (and export no
`WHARFY_SIGN_IDENTITY`): wharfy signs nothing but still *respects* your binaries — codesign them
yourself first, drop them at the `prebuilt:` paths, then `wharfy release` archives them verbatim (the
checksums are of your signed bytes). So the interim workaround for a signing bug is simply to
pre-sign and unset the identity. Note the asymmetry that keeps releases honest: a *configured*
identity that then fails to sign is a **fatal** error (the pipeline stops rather than shipping an
unsigned CLI), whereas *no* identity is a deliberate, allowed passthrough.

The GitHub Release itself (archives, deb/rpm, `install.sh`, `install.ps1`, `latest.json`) is
produced by `release`, not `publish` — direct download and `curl | sh` / `irm | iex` install
come from there, and the owned channels above reuse it. (`wharfy publish` only accepts the
channels listed here.) By default the script URLs point at the latest GitHub Release asset; set
`script: { base_url: … }` to advertise `install.sh` from your own domain or CDN instead (the
install instructions and the `status` probe follow that URL).

**When an install script fails, it hands you the next move.** The scripts are usually the first
and only thing a user of your project touches, so they never fail silently and never end on a raw
OS error. A failed download prints the URL, curl's exit code, and a manual download-and-unpack
recipe; an unwritable prefix prints two runnable one-liners — install where you can write, or run
the same command elevated. They never retry, never fall back to a mirror, and never elevate for
you: being unable to write somewhere is a question for the user, not a thing to work around. Their
exit codes are stable, so a coding agent watching the install can branch on them:

| exit | meaning |
|---|---|
| `0` | installed |
| `1` | unexpected failure — unclassified, please report |
| `2` | unsupported platform (os/arch) |
| `3` | download failed (dns / tls / proxy / http error / missing asset) |
| `4` | cannot write to the install prefix (permission, read-only fs, no space) |

Only these four meanings exist. Anything else that goes wrong is normalized to `1` and says so,
rather than borrowing a code it does not mean.

## Winding a channel down

Retiring a channel is a **state**, not a deletion. Keep it in `channels:` and say so — remove it and
wharfy stops touching it, which means it can no longer carry your notice to the users who are still
on it. You write the words; wharfy decides where they go.

```yaml
channels: [homebrew, script, goinstall]   # leave it in

deprecate:
  script:
    since: "1.4.0"
    ship: true          # default: keep shipping while you migrate. false freezes the last version
    message: |
      The install script is going away in 1.4.0. Please use Homebrew instead.
```

Your message is carried **verbatim** — wharfy never writes it, rewrites it, or wraps it in its own
words. It reaches users by two paths, because either one alone leaves someone behind:

| who | how |
|---|---|
| people installing now | the channel's own notice field — Homebrew/Cask `caveats`, Scoop `notes`, deb/rpm description, AUR `post_install`, and the install scripts print it after a successful install |
| people who installed already | `latest.json`, which your CLI already polls for updates — `caveats` only ever appears at install time |

Some channels have nowhere to put a notice: a Go module has no description field, and neither does a
container image. wharfy will not pretend otherwise — `status` and `publish` tell you that the notice
did not land there and reaches those users only through `latest.json`. Declaring `deprecate:` for
such a channel is allowed, not an error; you may decide `latest.json` is enough.

### `ship: false` — stop shipping, keep announcing

With `ship: false` the channel stays at the last version you published there. No new version reaches
it, ever. How far the notice still travels depends on what the channel lets wharfy rewrite:

| channels | what happens |
|---|---|
| homebrew, cask, scoop, aur | the manifest is rebuilt at the frozen version — same binaries, new notice |
| script | `install.sh` / `install.ps1` keep installing the frozen version and print the new notice |
| apt, rpm, container, winget, homebrew-core | nothing is written; what you already shipped stays, and the notice reaches users via `latest.json` |
| releases, goinstall | cannot be frozen — a Release is where every other channel's assets live, and `go install` always resolves the newest tag |

Rebuilding a manifest needs the checksums of the frozen version's assets, so wharfy records them
alongside each publish. A channel frozen before it was ever published has no version to freeze at:
wharfy ships nothing rather than leak a new version through a route you closed. Every publish says
what it froze — freezing shows up as an absence, and an absence is easy to mistake for success.

Writing no `deprecate:` block leaves every generated artifact byte-for-byte identical.

Take a channel **out** of `channels:` and wharfy stops publishing to it — including when you name it:
`wharfy publish homebrew` on a config without `homebrew` fails with `channel_not_configured` rather
than writing to a repository you retired (people usually archive it and leave a hand-written notice in
the last formula). Nothing suggests it either: `next` only ever proposes channels you declared.
Bringing it back is a deliberate edit to `channels:`, the same explicit gate as every other write.

`release` also writes a static `latest.json` (version + per-OS/arch asset URLs) to the same
Release, so its stable URL
`…/releases/latest/download/latest.json` is a vehicle-independent "is there a newer version?"
check any product can poll — reading it and prompting the user is the product's job (see
`schemas/latest.json` for the contract).

Run `wharfy agent --json` for the live set and each channel's kind.

Each channel needs its own prerequisites (a token, a hosted repo, docker, an AUR key, …).
wharfy tells you what's missing: `publish --dry-run` lists a `requires` block, and unconfigured
channels are skipped (not failed) in a batch. Owned tap/bucket repos are created for you on
`--yes` (a `tap_will_be_created` warning previews it).

### Releasing from CI

wharfy never prompts and never needs a TTY: `--yes` is the whole gate, and every credential comes
from the environment. The same commands run on your laptop and in a GitHub Actions workflow — and
for open source, building what you distribute in public CI is the better default (the artifacts
are auditable, cross-OS, and no signing key has to live on anyone's machine).

What the runner needs follows from your `channels:`, so wharfy works it out for you:

```sh
wharfy secrets          # credentials to register, tools to install, and the permissions:/env: to paste
```

It lists the tools too, not just the secrets. The Go build path shells out to **goreleaser**, and
the `container` channel to **docker** — both are on your machine already, which is exactly why the
first CI run is where you find out the runner has neither. BYO inputs (`prebuilt:` / `bundle:`)
never call goreleaser at all. And because the git tag *is* the version, `actions/checkout` needs
`fetch-depth: 0`: its default shallow clone carries no tags.

One thing it will tell you that is easy to get wrong: the token GitHub Actions hands a workflow
can only write to **that** repository. Channels that write elsewhere — `homebrew`/`cask` (your
tap), `scoop` (your bucket), `winget` and `homebrew-core` (a fork) — need a PAT registered as a
secret and passed as `GITHUB_TOKEN`. Channels that stay in your own repo (`releases`, `script`,
`goinstall`, `container` on ghcr) run on the built-in token with the right `permissions:`.

What stays deliberate is the trigger, not the machine: ship on a tag push or a manual dispatch,
never on every merge (`wharfy init` writes that discipline into your `AGENTS.md` / `CLAUDE.md`).

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

If your binary shells out to another tool at runtime (the package manager installs it for you —
your binary doesn't bundle it; go.mod deps are baked into the binary and need no declaration),
declare it so "the usual install" pulls it in too. Declare only the **first hop** your binary
calls directly — each tool's own deps resolve transitively.

Prefer the cross-channel `runtime_deps`: declare once, projected to all owned package channels
(homebrew / scoop / apt / rpm / aur):

```yaml
runtime_deps:
  - name: ffmpeg                 # same name everywhere → one line
    min: "6.0"                   # apt `(>= 6.0)` / rpm `>= 6.0` / aur `>=6.0`; brew & scoop degrade to name only
  - name: fzf
    required: false              # → apt/rpm recommends, aur optdepends; brew/scoop omit optional deps
  - name: rg
    as: { apt: ripgrep, homebrew: "node => :recommended" }  # per-channel verbatim override (names differ / channel-native syntax)
```

wharfy can't know a distro's real package name, so it does **not** absorb naming differences:
the common case is the same name everywhere (one line); when a name differs or you need
channel-native syntax, the `as` value is emitted **verbatim** for that channel — so the worst
case equals hand-writing the dependency line yourself (no lock-in). `min` degrades to name-only
on homebrew/scoop (they can't express version constraints — that's expected, not an error);
`wharfy verify` is the real check that the dep actually installs.

Per-channel fields still work and merge (union) with `runtime_deps` for back-compat:

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

CI runs gofmt / vet / build / `go test -race` on every push and PR, and repeats `go test` on
`windows-latest` and `macos-latest` as their own jobs. Every OS wharfy claims to ship to is an OS CI
actually walks: only on Windows is `install.ps1` run by the PowerShell your users have, and only on
macOS does `install.sh` meet the BSD `install` / `tar` / `uname` it will meet in the wild.

One suite is behind a build tag, because it needs a real docker and a real network. It builds
deliberately broken `.deb` and `.rpm` packages — an unmet dependency, a binary off `PATH` — serves
them from a local apt/rpm repo, and checks that `wharfy verify --install` fails on them (and passes
on a healthy one). Everything else stubs docker out, so this is the only place the container step
itself is exercised. CI runs it as its own `docker-verify` job, so the fast suite above is never held
up by image pulls and installs.

```sh
go test -tags dockerverify ./cmd/wharfy/ -run TestDockerVerify -timeout 20m
```

## License

[AGPL-3.0](LICENSE)
