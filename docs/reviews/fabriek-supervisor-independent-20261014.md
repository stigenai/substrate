# Independent protected-supervisor review

Reviewed runtime `aea519a7` and the final non-semantic/test follow-ups through
`4da6b03a` against baseline `9f860626`. Result: PASS for the source and local
component scope; no unresolved blocking finding.

The issuer authenticates the calling atelet, checks the current actor activation,
worker assignment and current Node/Pod UIDs, binds the actual template projection,
and rereads actor version before issuing a dedicated short-lived audience token.
The generic actor JWT path cannot request supervisor claims. DATA restoration
refreshes the protected projection before execution; protected actor memory
snapshots are rejected, with the explicitly credential-free initial golden image
exception. Ordinary workload and generic actor-token paths retain their behavior.

Independent checks in a clean worktree:

- Full race suites for supervisorbootstrap, workerservice, controlapi, ateletauth,
  and atelet: PASS (including the control API's PostgreSQL tests).
- Ordinary actor JWT, template validation and OCI compatibility selection: PASS.
- Real interceptor supervisor-token redaction tests: PASS.
- Final `proto.Equal` simplification: reviewed, no semantic change.

Logs are retained under `/tmp/fabriek-native-admission-20261014/` as
`substrate-bootstrap-independent.log`,
`substrate-bootstrap-compatibility-independent.log`, and
`substrate-bootstrap-redaction-independent.log`.

Hosted gVisor execution, real snapshot isolation, deployment and production
identity qualification remain NOT_RUN by this review. These local checks do not
claim a completed Fabriek run or enable its AX profile.

Implementation verification at runtime source `4da6b03a` also passed the complete
repository `make verify`, with `NO_COLOR` unset and `GOFLAGS=-p=2` to avoid the
host's color override and excessive parallel test load. All verifiers remained
enabled, including race tests, generated code, lint, licenses, schema checks,
Python proto generation and shell checks. The log is
`/tmp/fabriek-native-admission-20261014/substrate-verify-final-4da6.log`.
`go build ./cmd/ateapi ./cmd/atelet ./cmd/kubectl-ate` and focused `go vet` passed.
An actual synthetic ES256 token issued by the Go service passed Fabriek's Python
verifier at its issuance time plus ten seconds. Review-receipt commits after
`4da6b03a` change documentation only.
