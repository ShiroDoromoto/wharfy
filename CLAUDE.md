<!-- wharfy:begin (managed) -->
## Releasing

Release and distribution for this project go through **wharfy**.
Don't guess the steps — run `wharfy agent` first (agents: `wharfy agent --json`)
and follow its output. That capability map is always current.

wharfy is non-interactive: `--yes` needs no TTY and every credential comes from the
environment, so `wharfy release` / `wharfy publish` run the same on a laptop and in a
GitHub Actions workflow. Building the artifacts you distribute in public CI is fine —
for open source it is the better default.

What stays deliberate is the trigger, not the machine. Ship on a tag push or a manual
dispatch — never on every merge. Auto-merging dependency bumps (Dependabot etc.) is
fine; let them accumulate, then ship on purpose.
<!-- wharfy:end -->

<!-- amenbo:begin (managed v2) -->
# amenbo — guide for AI agents

This block is managed by amenbo; do not edit between the markers (your own content outside
them is preserved). This directory is managed with amenbo (a local-first, server-less task
manager). amenbo never reads or writes the project's own contents (your source and files);
it only lets agents launched in this folder operate the amenbo backlog. The only files it
places here are `.amenbo` (a pointer to the store) and managed blocks like this one.

Run `amenbo agent --json` for the full command spec, workflow, and rules (the single source
of truth). Always set `AMENBO_ACTOR=ai`.

**Communicate with the human, and write task titles, notes, and comments, in Japanese.**
<!-- amenbo:end -->
