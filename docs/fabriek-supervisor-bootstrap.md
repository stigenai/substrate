# Fabriek supervisor bootstrap

This opt-in platform boundary authenticates one native supervisor activation. It
does not grant model, tool, source, or publication authority. Fabriek owns the
one-use nonce exchange, enrollment, boot permits, and current-effect checks.

## Projection and lifecycle

An ActorTemplate opts in with a SystemInfo data source
`supervisorBootstrap: {profile: fabriek-native-v1}`. It must be the only data
source in that volume, mounted only by the single root container at
`/run/substrate/fabriek-supervisor`. The file `activation.jwt` is root-owned
0600 in a root-owned 0700 directory, read-only to the guest. No secret appears in
Task environment, actor metadata, or the public template. Unsupported profiles
fail closed. The template must set `snapshotConfig.preferredFidelity` to
`VOLUMES` or `ROOTFS`, so no snapshot holds process memory and every resume
cold-boots the supervisor. Resume refuses an actor whose snapshot to restore
holds memory. The golden actor, which the template reconciler names after the
template UID, receives an empty protected volume and no token. Files in
SystemInfo are excluded from snapshots, but file exclusion alone does not
exclude bytes read into memory, which is why memory snapshots are refused.

The server records `status.supervisorActivation` when it transitions the actor
to RESUMING under its existing lifecycle lease. It contains `activationId`,
`workerUid`, `templateUid`, `templateDigest`, `imageDigest`, `launchDigest`, and
`profile`. A retried RESUMING operation retains the same identity; a subsequent
activation receives a new UUID even on the same worker. Missing or mismatched
identity denies issuance. Public status never contains the JWT. Actor records
already persist as protobuf blobs, so this additive field needs no SQL migration.

## Restricted issuance

`WorkerService.MintSupervisorBootstrap` takes actor reference, actor UID,
activation ID, and worker UID. Only atelet mTLS is accepted. Issuance verifies
current actor/worker assignment, the calling atelet's node incarnation, live
worker and atelet Pod identities, approved template projection, and RESUMING/RUNNING state.
The secret is delivered only to atelet, which projects it before starting or
restoring the workload. Generic `Control.MintActorJwt` cannot emit this claim.

JWTs reuse the configured actor signing pool and issuer. Standard claims are
`iss`, `sub=actor/<atespace>/<name>`, `aud=[urn:fabriek:native-bootstrap]`,
`iat`, `nbf`, `exp`, and random `jti`; the lifetime is 300 seconds. The
`actor.ate.dev` claim identifies `atespace`, `actorName`, and `actorUID`. Its
additional `supervisor` object uses these exact JSON keys:

```json
{
  "schema_version": "substrate.supervisor-bootstrap/v1",
  "purpose": "fabriek-supervisor-bootstrap",
  "profile": "fabriek-native-v1",
  "activation_id": "<UUID>",
  "worker_uid": "<Pod UID>",
  "node_name": "<current node>",
  "node_uid": "<current node UID>",
  "template_uid": "<template UID>",
  "template_digest": "sha256:<deterministic template protobuf hash>",
  "image_digest": "sha256:<worker OCI digest>",
  "launch_digest": "sha256:<immutable Fabriek launch hash>"
}
```

The template digest hashes deterministic protobuf with metadata and status
removed; the launch digest comes from the template's single
`FACTORY_NATIVE_LAUNCH_DIGEST` entry. Fabriek compares both to retained dispatch
authority and its qualified image/profile. A valid signature alone is insufficient.
Verification uses operator-pinned public signing keys and issuer, never
request-controlled discovery. The existing signing pool exposes public keys;
this change does not introduce a new issuer or assume a deployed discovery server.

## Fabriek exchange

The root supervisor reads the projected token and generates its fresh RAM boot
nonce after observing the actual activation, including after a credential-free
golden restore. It POSTs `/native/worker/bootstrap` with
`{attempt_id, launch_digest, boot_id}` and the token in Authorization. Fabriek
verifies the signature/claims and obtains the current Actor over its protected
platform read path. Current status must be RUNNING with exactly the signed
actor UID, activation, worker and template binding. A durable one-use exchange
binds activation to attempt, launch and boot nonce; a different nonce cannot
reuse it. Retried identical requests may return the same enrollment, but do not
authorize another native launch. Subsequent effects still require Fabriek's
current accepted permit and the current platform activation. The platform token
is only bootstrap authority.

A supervisor restart within one activation requires a new platform activation;
it cannot claim a second boot by presenting the same projection. Old activation,
actor recreation, suspend, expired identity, changed template, missing current
state, unavailable observation and unqualified image deny. Native UID 10001 must
not read the projection, supervisor memory, or controller credential.

## Qualification boundary

Unit and real-store tests establish schema, authorization and replay behavior.
Actual gVisor tests must establish UID isolation, root projection permissions,
memory-free snapshots, restore freshness and old-activation denial.
Deployment also requires exclusive AX writers/private API reachability and
separate approved Claude/Codex account, tool, revocation and egress evidence.

## Local verification

The implementation has focused tests for actual ES256 signatures and exact
claims, stale actor/activation/worker/node denials, deleted atelet and worker
Pods, missing configuration, and secret-response log redaction. Projection
tests exercise 0700/0600 modes, missing issuer rejection without creating a
token file, and an empty projection that never calls the issuer. These use
synthetic Kubernetes identities and local files, not a hosted gVisor actor.

Real PostgreSQL tests retain the activation before workload conversion and
rotate it across a new lifecycle activation while keeping the actor UID. They
also cover the golden actor receiving no activation and the refusal to restore
a memory snapshot. Existing ordinary actor and SystemInfo tests remain required.

Actual gVisor UID isolation, snapshot/restore and deployed identity qualification
are **NOT_RUN** at this source checkpoint. No production authority or account
qualification follows from the local test results.

## Exact termination proof

Protected activations carry their complete server-owned identity through Run and
Restore to the worker. The worker retains a cloned binding before launch. A
protected Terminate request must match that binding, actor UID, actor directories
and runtime arguments. Its `substrate.supervisor-stop/v1` acknowledgement is
returned only after the actor's actual cgroup has been frozen, its live processes
pinned by pidfd, killed, observed unpopulated and reaped, and runtime cleanup has
succeeded. Supported runtime values are exactly `gvisor` and `microvm`.

Atelet validates and propagates this acknowledgement only after its own cleanup
succeeds. The control plane retains it with the original worker UID and node
before releasing the assignment. The final exact UID actor deletion and
`substrate.supervisor-termination/v1` receipt insertion share one PostgreSQL
transaction. Migration 000002 makes receipts immutable and independent of the
actor row. A controller restart can reuse the retained acknowledgement; a lost
runtime or atelet acknowledgement before retention cannot be reconstructed from
absence. A worker that is gone or unreachable returns no acknowledgement, so the
deletion completes without a receipt.

Deleting an actor that holds an activation requires an exact UID precondition
before any state mutation. An old name-only deletion cannot delete a protected
replacement actor; ordinary actor deletion still accepts its existing options.

Read proof over the existing authenticated private Control connection:

```sh
kubectl-ate get supervisor-termination ACTOR --atespace SPACE \
  --actor-uid RETAINED_UID --activation-id RETAINED_ACTIVATION -o json
```

The command has a ten-second deadline and a 16 KiB public JSON output bound. It
uses `ReadSupervisorTermination`, which the registry authorizes as a read of the
actor. The JSON uses protobuf snake_case names and RFC3339 timestamps. It
contains the exact actor namespace, name and UID, the complete activation, node
name, host-stop acknowledgement, deletion UUID and deletion timestamp. The
command rejects a receipt whose deletion time precedes its host-stop timestamp.
Consumers compare all retained identity fields, including worker, template,
image and launch.

Lookup returns no proof while **any current actor** occupies that namespace and
name, even when an older immutable receipt exists. This is a single database
snapshot observation, not a fence against a future creation. The caller must
also retain its exact AX Task deletion identity and prevent queued
reconciliation from recreating that task. No TaskNotFound, ActorNotFound,
missing worker, skipped termination, empty cgroup, expired activation or
successful generic deletion is substituted for a host-stop acknowledgement.
Ordinary actors retain their previous cleanup behavior and produce no protected
receipt.

Local verification uses real PostgreSQL transactions, authorization checks,
atelet RPC lifecycle fixtures and Linux kernel and process tests. The worker
tests use synthetic retained activation bindings. They do not establish hosted
gVisor or micro-VM guest qualification, which stays **NOT_RUN** until the
separately approved candidate pool runs it.
