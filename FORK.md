# The stigenai fork

This repository is a fork of
[agent-substrate/substrate](https://github.com/agent-substrate/substrate).
We keep our changes as a **patch stack**. A patch stack is an upstream
release tag, with a linear series of our commits on top of it.

| Item | Value |
|---|---|
| Fork branch | `stigenai/main` |
| Base | An upstream release tag. `hack/fork/fork.sh base` prints it. |
| Upstream remote | `upstream` = `ssh://git@github.com/agent-substrate/substrate` |
| Fork release tags | `<base>-stigenai.<n>`, for example `v1.0.1-stigenai.1` |
| Tools | `hack/fork/fork.sh` |

## Rules for fork commits

1. Each commit is one change with its tests. Put a Go test in the package
   that it tests, in the same commit as the change.
2. Each commit has two trailers. `hack/fork/fork.sh check` fails without them.

   ```text
   Fork-Topic: egress-paths
   Fork-Upstream: candidate
   ```

   | `Fork-Upstream` | Meaning |
   |---|---|
   | `candidate` | General fix or feature. Send it upstream as a PR, then drop it here. |
   | `fork-only` | stigenai-specific: our fleet, our deploy, our product. |
   | `test-only` | A test that guards upstream behavior we depend on. |

3. Keep the commits of one topic together in the stack.
4. Add new files when you can. Change an upstream file only when you must.
   A new file cannot cause a conflict on a rebase.
5. Do not merge upstream into `stigenai/main`. Rebase the stack onto a new tag.

To add the trailers to a commit:

```bash
git commit --amend --no-edit --trailer 'Fork-Topic: k0s' --trailer 'Fork-Upstream: fork-only'
```

## Show the stack

```bash
hack/fork/fork.sh base     # for example v1.0.1
hack/fork/fork.sh series   # hash, topic, upstream status, subject
hack/fork/fork.sh check
```

## Move to a new upstream release

1. Get the new tags:

   ```bash
   git fetch upstream --tags
   ```

2. Make a branch for the rebase. Then run the command below to replay the
   stack. The script turns on `rerere`. Git then records each conflict
   resolution and uses it again.

   ```bash
   git switch -c stigenai/rebase-v1.0.2 stigenai/main
   hack/fork/fork.sh rebase v1.0.2
   ```

3. If a commit conflicts, do these actions:
   - Resolve the conflict.
   - Run `git add`.
   - Run `git rebase --continue`.

   If upstream already has the change, run `git rebase --skip`.
   If its tests pass on the upstream code, keep them in a `test-only` commit.
4. If a commit changes a `.proto` file, run `hack/update/codegen.sh` after
   you resolve it. Do not resolve conflicts in generated `.pb.go` files by
   hand. If a commit changes `go.mod`, run `hack/update/go-modules.sh`.
5. Compare the old and the new stack. The script prints the command:

   ```bash
   git range-diff v1.0.1..<old-head> v1.0.2..HEAD
   ```

6. Run the checks:

   ```bash
   go build ./... && go test -race ./... && hack/verify-all.sh
   hack/fork/fork.sh check
   ```

7. Push `stigenai/rebase-v1.0.2` and open a PR to `stigenai/main` for review.
   Put the `range-diff` output in the PR description. Do not merge the PR.
   After the approval, force-push the rebased head to `stigenai/main`:

   ```bash
   git push --force-with-lease origin stigenai/rebase-v1.0.2:stigenai/main
   git tag v1.0.2-stigenai.1 && git push origin v1.0.2-stigenai.1
   ```

## Review the stack as patch files

```bash
hack/fork/fork.sh export ../substrate-patches
```

This command writes one `.patch` file for each commit. It also writes a
`series` file. The `series` file shows the topic and the upstream status of
each patch. After each rebase, commit the export to the `stigenai/patches`
branch. The history of that branch then shows how each patch changed from one
base to the next.

## Send a candidate upstream

1. Make a branch from `upstream/main`. Then cherry-pick the topic commits on that branch.
2. Remove the trailers. Then follow the upstream `CONTRIBUTING.md`.
3. When upstream releases the change, the next rebase drops our commit.
4. Set the register status below to `proposed <link>`, then to `merged`.

## Patch register

Principle V of `.specify/memory/constitution.md` requires this register. It
lists each topic that edits upstream files. The status of each topic is
`fork-only`, `candidate`, `test-only`, `proposed <link>` or `merged`.

`hack/fork/fork.sh series` shows the commits of each topic. Topics that have only
new files have no entry: `aws`, `static-worker` and `layerfs-e2e`.

### fork-meta

- **Status:** `fork-only`
- **Upstream files:** `hack/util/verify-boilerplate.py` and
  `hack/third_party/kubernetes/verify-shellcheck.sh`. Each skips `.specify/`.
- **Reason:** `specify init` writes the Spec Kit files. They have no license
  header, and their scripts fail shellcheck. A new `specify init` writes over
  any header that we add.
- **Remove when:** the fork stops using Spec Kit.

### k0s

- **Status:** `fork-only`
- **Upstream files:** `cmd/ate-setup/internal/config/{config,load,setting}.go`,
  `cmd/ate-setup/internal/steps/{delete,deploy,overlay,postgres}.go`,
  `manifests/ate-install/atenet-egress.yaml` and
  `cmd/ate-setup/internal/steps/egress_dns_test.go`.
- **Reason:** `ate-setup` cannot select an install profile directory.
  `ATE_INSTALL_PROFILE=k0s` and `ATE_INSTALL_PROFILE=aws` use this switch. The
  fleet has no IPv6 egress, so the egress DNS cache uses `V4_PREFERRED`.
  Upstream uses `ALL`.
- **Remove when:** upstream adds a profile switch and a setting for the DNS
  address family.

### aws

- **Status:** `fork-only`. It has new files only.
- **Install:**

  ```bash
  ATE_INSTALL_PROFILE=aws go run ./cmd/ate-setup --kind deploy ate-system \
    --image-repo REPO --image-tag TAG \
    --storage-bucket-name BUCKET --snapshot-location s3://BUCKET/
  kubectl apply -f manifests/ate-install/aws/sandboxconfig-gvisor.yaml
  ```

- **Values to edit before an install.** Each value has an `EDIT` mark:

  | File | Value |
  |---|---|
  | `manifests/ate-install/aws/ate-api-server/kustomization.yaml` | Role ARN and region |
  | `manifests/ate-install/aws/atelet/kustomization.yaml` | Role ARN and region |
  | `manifests/ate-install/aws/sandboxconfig-gvisor.yaml` | S3 URI of the gVisor mirror |

### devenv

- **Status:** `fork-only`
- **Upstream files:** `.gitignore`, `hack/protoc.sh` and
  `hack/util/verify-boilerplate.py`, which skips `devenv.lock`.
- **Reason:** the fleet builds in a cached devenv. `hack/protoc.sh` must stop
  with an error when an HTTP error occurs. It must not save the error page.
- **Remove when:** the fleet stops using devenv.

### image-pull-keychain

- **Status:** `proposed https://github.com/agent-substrate/substrate/pull/4183`
- **Upstream files:** `cmd/atelet/main.go`
- **Reason:** atelet pulls images with the GCP keychain only. A non-GCP fleet
  needs the Docker config keychain.
- **Remove when:** upstream merges it.

### atelet-dialer

- **Status:** `proposed https://github.com/agent-substrate/substrate/pull/4184`
- **Upstream files:** `cmd/ateapi/internal/controlapi/dialer.go` and its tests.
- **Reason:** the v1.0.1 dialer keeps the old connection open. This happens when
  Kubernetes replaces the atelet pod, when the node has no atelet, or when the
  atelet has no IP.
- **Remove when:** upstream merges it.

### microvm-fuse

- **Status:** `candidate`
- **Upstream files:** the WorkerPool API types and the generated deepcopy and
  CRD, the WorkerPool controller, `cmd/ateworker-microvm`, `internal/ocispec`,
  `docs/api-guide.md` and the e2e probe.
- **Reason:** a microVM actor must mount a FUSE file system, for example
  LayerFS. The guest has no `/dev/fuse`, and the actor cannot `mknod` it. The
  operator uses `WorkerPool.spec.guestDevices` to turn on the device.
- **Remove when:** upstream merges it, or adds a different device API for
  microVM actors.

### native-tls-gateway

- **Status:** `fork-only`
- **Upstream files:** `cmd/atenet/internal/router/{cmd,config,router}.go`,
  `cmd/atenet/internal/router/egress/egress.go`,
  `internal/egresspolicy/egresspolicy.go` and `.github/workflows/pr-workflow.yaml`.
- **Reason:** the Fabriek native-TLS plane needs one fixed destination for each
  gateway. The actor sees the certificate of the origin. The gateway also makes
  sure that the actor UID in the certificate is the UID of the actor that runs.
  It is an addition to the upstream `tls_passthrough`.
- **Remove when:** upstream passthrough can pin one destination for each
  gateway and need an identity that binds the actor UID.

### egress-tls-pools

- **Status:** `candidate`
- **Upstream files:** `manifests/ate-install/atenet-egress.yaml` and
  `egresspolicymanifest_test.go`.
- **Reason:** Envoy offers a cached TLS session to a different SNI. Origins
  behind Cloudflare then return 503. `max_session_keys: 0` on the two upstream
  TLS clusters stops this.
- **Rebase:** after v1.0.1, upstream added the TLS cluster
  `egress_forward_proxy_websocket` (upstream #3985). On a base that has this
  cluster, set `max_session_keys: 0` on it too. Also add it to the cluster list
  in `TestEgressManifestsDisableUpstreamSessionResumption` and in
  `upstreamTLSClusters`. If you do not, the manifest test fails. The branch
  `up/egress-tls-pools` has the change.
- **Upstream:** issue #3805 and PR #1764 are open. Comment on the issue. Do not
  open a new PR.
- **Remove when:** upstream sets `max_session_keys: 0`, or Envoy keys the
  sessions by SNI. Envoy v1.40 keys them by SNI (envoyproxy/envoy#45982).

### egress-paths

- **Status:** `candidate`
- **Upstream files:** `api/ateapi/v1/ateapi.proto` and the generated code,
  `cmd/ateapi/internal/apivalidation/egress_policy.go`,
  `internal/egresspolicy/egresspolicy.go`,
  `cmd/atenet/internal/router/egress/request.go`, `go.mod` and
  `docs/guides/egress-credential-injection.md`.
- **Reason:** two HTTPS rules for one host cannot inject different
  credentials. `HTTPSRule` adds path prefixes and exact `paths`. The check
  rejects paths that a server can decode to a different path.
- **Rollout:** Do not create a policy that uses `paths` before every atenet
  runs this commit. An older atenet ignores `paths`. The rule then applies to
  all paths on the host.
- **Remove when:** upstream merges it, or adds a path matcher that does the
  same work.

### fabriek-bootstrap

- **Status:** `fork-only`
- **Upstream files:** the activation and bootstrap fields in `ateapi`,
  `ateletpb` and `ateworkerpb`; the issue path in `workflow_resume`,
  `workerservice`, `ateletauth` and `actoridjwt`; atelet validation and the
  snapshot exclusion.
- **Reason:** upstream has no hook for an activation identity or a projected
  token for each actor. The fields must be in the shared messages.
- **Remove when:** upstream adds a bootstrap for protected workloads, or we
  retire the Fabriek supervisor.

### supervisor-termination

- **Status:** `fork-only`
- **Upstream files:** the Terminate response path through the workers, atelet
  and the control plane; the delete workflow; `atepg` and migration
  `000002_supervisor_termination.sql`; the `Control.ReadSupervisorTermination`
  RPC and its authz rule; the cgroup stop.
- **Reason:** delete and terminate are fixed steps in the core workflow. The
  receipt must be written in the same transaction as the actor delete.
- **Remove when:** upstream adds a verified stop or a deletion receipt, or we
  stop using protected actors.
