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
fail closed. The template must use Data on pause and commit, and Golden for
Data resume. Golden actors receive an empty protected volume and no credential.
A non-golden actor cannot restore a previously activated actor's Full memory
snapshot. Its initial template-owned, credential-free golden snapshot and an
idempotent retry of that same RESUMING activation are the only Full exception.
Files in SystemInfo
are excluded from snapshots; keeping golden memory credential-free is separately
required because file exclusion alone does not exclude bytes read into memory.

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
restoring the workload. Generic `Control.MintActorJWT` cannot emit this claim.

JWTs reuse the configured actor signing pool and issuer. Standard claims are
`iss`, `sub=actor/<atespace>/<name>`, `aud=[urn:fabriek:native-bootstrap]`,
`iat`, `nbf`, `exp`, and random `jti`; the lifetime is 300 seconds. Existing
`ate.dev` fields identify `atespace`, `actorName`, and `actorUID`. The additional
`supervisor` object nested inside `ate.dev` uses these exact JSON keys:

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
credential-free golden capture, Data restore freshness and old-activation denial.
Deployment also requires exclusive AX writers/private API reachability and
separate approved Claude/Codex account, tool, revocation and egress evidence.

## Local verification

The implementation has focused tests for actual ES256 signatures and exact
claims, stale actor/activation/worker/node denials, deleted atelet and worker
Pods, missing configuration, and secret-response log redaction. Projection
tests exercise 0700/0600 modes, missing issuer rejection without creating a
credential file, and a golden projection that never calls the issuer. These
use synthetic Kubernetes identities and local files, not a hosted gVisor actor.

Real PostgreSQL tests retain the activation before workload conversion, retain
it across a retry, and rotate it across a new lifecycle activation while keeping
the actor UID. They also cover credential-free golden restore and Full actor
snapshot denial. Existing ordinary actor and SystemInfo tests remain required.

Actual gVisor UID isolation, snapshot/restore and deployed identity qualification
are **NOT_RUN** at this source checkpoint. No production authority or account
qualification follows from the local test results.
