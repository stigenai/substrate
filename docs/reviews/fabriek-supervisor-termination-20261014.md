# Protected supervisor termination verification

Runtime source: `62dfcf6a970720c9cc3e803bd70eb956000d1ea6`, with test-only
contract follow-up `867830059a73949ba5ffe4289d3b9aa932729a1a`, based on accepted
`9d8b18ee`. The runtime slice is `feb70dee907c245082f00db882e981f141460af2`
(cherry-picked as `e3fb602d`), with the protocol in `6288086e`.

The control plane retains an exact host acknowledgement before releasing the
worker and commits the actor deletion with its immutable receipt. Protected
deletion requires the actor UID before changing state. Receipt lookup uses the
existing private authenticated Control API and denies any current occupant of
the old actor name. This observation does not fence future actor creation; AX
must separately retain exact deletion identity and prevent new reconciliation.

## Local evidence

The complete corrected `make verify` command passed at
`867830059a73949ba5ffe4289d3b9aa932729a1a`, exit 0, with a clean worktree.
It ran the standard Go race suite and every repository verifier: generated
code, module files, format, lint, licenses, metrics, migrations, proto and
Python clients, shell scripts, and threat documentation. The nonverbose Go
summary reports 123 passing packages and 64 packages with no test files; it
does not enumerate individual test or skip counts. Focused counts below come
from their verbose evidence and overlap; they must not be added together.

Command, with the repository's existing pinned clang tools available on `PATH`:

```sh
env -u NO_COLOR GOFLAGS=-p=2 GOMAXPROCS=4 REQUIRE_DOCKER=1 make verify
```

- Independent source review: PASS, no blocking findings at `62dfcf6a`.
- Independent original pure/store/control/atelet/CLI assertions: 24 top-level
  tests / 101 including subtests PASS, zero skipped or failed.
- Independent full actual OpenFGA authorization package: 9 top-level / 25
  including subtests PASS, zero skipped or failed. The new receipt route denies
  absent and unauthorized principals and permits the authorized principal.
- Default Docker race partition contract: 250 cases including subtests PASS,
  zero skipped or failed. The standard suite first caught the new receipt table
  missing from the store contract. The follow-up exercises actual insertion and
  lookup in both partition layouts, preserving every partition check.
- Final control workflow and ordinary deletion compatibility: 9 top-level / 27
  including subtests PASS. This executes DeleteActor, a synthetic bound host ACK,
  atomic PostgreSQL receipt retention, and the actual RPC lookup. Empty or stale
  protected UID is rejected before state mutation, including before activation
  and with a missing template after activation.
- Runtime Linux kernel/process suite: independent retry PASS, 95 top-level / 182
  including subtests, zero skipped or failed. The first attempt timed out in the
  existing `TestKillActorLeaf`; its log is retained. The new protected helper
  tests passed on that attempt, and the runtime suites were not reached.
- Linux amd64 `ateapi`, `atelet`, and `kubectl-ate` build PASS from the exact
  source; binary hashes are retained in `substrate-termination-linux-build.json`.
- Affected packages compile; pinned Linux lint reports zero issues. Clean
  repository Go/proto formatting, full generated-code verification, boilerplate,
  PostgreSQL migration validation and Markdown formatting passed.

The initial complete `make verify` attempt was blocked by a Docker stall:
PostgreSQL container startup timed out, and only the owned blocked test process
trees were stopped. The first retry after Docker recovered exposed the missing
store contract coverage, corrected by `86783005`. Both failed logs remain
retained separately from the final successful standard run.

Alternative PostgreSQL checks used a verified local PostgreSQL 17.11 server on
port 63221 and scratch Go test overlays changing only test connection setup.
Original assertions and production constructors and migrations were retained;
databases had unique owned names. No repository test harness or test was
disabled. The final successful standard run used the unchanged Docker harness.

Evidence is retained under `/tmp/fabriek-native-admission-20261014/`:

- `supervisor-termination-independent-review.json` and
  `termination-control-independent-local.log`.
- `substrate-termination-explicit-pg.log` and
  `termination-local-pg-overlay/overlay.json`.
- `supervisor-stop-independent.json`, `supervisor-stop-independent-retry.json`
  and their logs, plus `supervisor-stop-final.json`.
- `substrate-termination-partition-final.log`.
- `substrate-termination-verify-final.log` and
  `substrate-termination-verify-final.exit` (final complete PASS).
- `substrate-termination-verify.log`,
  `substrate-termination-verify-retry.log`, and
  `substrate-termination-docker-stall.json` (prior failures).
- `substrate-termination-final-compile.log`,
  `substrate-termination-lint-final.log`,
  `substrate-termination-guard-lint.log`, and
  `substrate-termination-format-final.log`, and
  `substrate-termination-codegen.log`.
- `substrate-termination-linux-build.json` and its build log.

The runtime tests prove real cgroup/process termination with synthetic retained
activation bindings and a runsc cleanup fixture. The control tests use synthetic
host ACKs. Neither establishes a hosted gVisor sandbox or microVM guest,
deployment, candidate-pool identity, model inference, or a completed Fabriek run.
Those qualification tiers remain NOT_RUN. No live switches or production state
were changed by these checks.
