<!--
Sync Impact Report
Version: (none) → 1.0.0
Added: all principles and sections. This is the first ratification.
Templates: there are no files in .specify/templates/* at this time. Run `specify init --here --ai claude` to add them.
-->

# stigenai/substrate Fork Constitution

This repository is a fork of `agent-substrate/substrate` (remote `upstream`).
Our fork is `stigenai/substrate` (remote `origin`).
The upstream `AGENTS.md`, `CONTRIBUTING.md` and `docs/dev/` rules apply to all work here.
This constitution adds the rules for the fork. If the two rules are different, upstream rules win for code that we can donate.

## Core Principles

### I. Upstream Compatibility First

- `main` MUST stay a fast-forward or clean merge of `upstream/main` plus a small set of fork patches.
- Each fork patch MUST touch the minimum number of upstream files.
- Do not reformat, rename or move upstream code in a fork patch. This causes merge conflicts and gives no value.
- Use an extension point if there is one. Do not add a fork-only flag, proto field or config key to an upstream type.

**Why:** Each line that is different from upstream is a merge cost that we pay at each sync.

### II. Put Fork Code Outside Upstream Code

Use the first alternative that works:

1. **No fork change:** use configuration, manifests or a public API.
2. **Integration repository:** if the code runs *on* Substrate, put it in a different repository (see `docs/integration-repos.md`).
3. **Plugin:** put it in `internal/plugins/<name>` with its own `go.mod`, and let it import only `pkg/` (see `internal/plugins/README.md`).
4. **New file in the fork:** add new files and do not edit upstream files.
5. **Edit an upstream file:** use this rung only if rungs 1 to 4 cannot work. Keep the edit small and record it in `FORK.md`.

### III. Upstream Small Changes

- If a change is small, general and useful to upstream users, write it so we can donate it.
- A change we can donate MUST follow all upstream rules. These rules cover tests, `make verify`, copyright headers, US spelling, no back-compat code and no issue references in commit messages.

- A change we can donate MUST NOT contain stigenai names, internal hostnames, credentials, customer data or fork-only paths.
- Keep each change we can donate in its own commit or branch, so it goes upstream without other fork work.
- If upstream merges the same change, remove our fork patch at the next sync.

### IV. A Human Does All Upstream Contact (NON-NEGOTIABLE)

- An agent MUST NOT open, comment on, or push to an upstream issue or pull request.
- An agent can prepare an upstream change: a branch, a commit and a draft PR description. Keep the draft PR description in `FORK.md` or a bead.

A human MUST do these steps:

1. Make sure that the author signs the Google CLA (`CONTRIBUTING.md`).
2. Discuss the change with upstream first in an issue, as `CONTRIBUTING.md` tells you.
3. Read the full diff and open the PR.
4. Answer the review comments.

- Do not report a security problem in a public upstream issue or PR. A human MUST report it to the upstream maintainers in private.

**Why:** A person sends each upstream PR with that person's name and CLA. A bad PR damages our relation with upstream.

### V. Record Each Divergence

- `FORK.md` at the repository root MUST list each fork patch that edits upstream files.
- Each entry MUST give the files, the reason, the upstream status and the removal condition. The upstream status is `fork-only`, `candidate`, `proposed <link>` or `merged`.
- If an entry has the status `merged`, remove the patch and the entry at the next sync.

### VI. Sync Often

- Merge `upstream/main` into `main` at least one time each week.
- After each sync, `make build`, `make test` and `make verify` MUST pass. Do not merge more fork work until they pass.
- If a fork patch conflicts on two or more syncs, examine it again. Then move it to a plugin, donate it, or delete it.

## Development Workflow

- Use speckit for feature work: `/speckit.specify`, `/speckit.plan`, `/speckit.tasks`, `/speckit.implement`.
- Each spec MUST give its target: `upstream-candidate` or `fork-only`. Each plan MUST name its rung of Principle II and tell why the higher rungs cannot work.
- Use beads (`bd`) for task tracking.
- Fork-only tooling (`.specify/`, `.beads/`, `.claude/`) stays in the fork. Do not include it in an upstream change.

## Governance

- This constitution applies to all specs, plans and reviews in this fork.
- A reviewer MUST reject a change that edits upstream files. The exception is an edit that Principle II gives a reason for.
- To change this constitution, edit this file in a PR, update the version and give the reason.
- Versions use semver: MAJOR removes or redefines a principle, MINOR adds a principle or section, PATCH makes the text clearer.

**Version**: 1.0.0 | **Ratified**: 2026-11-07 | **Last Amended**: 2026-11-07
