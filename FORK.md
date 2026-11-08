# Fork Patches

This file lists each fork patch that edits upstream files. Principle V in `.specify/memory/constitution.md` gives the rule.

## Micro-VM guest devices (`WorkerPool.spec.guestDevices`)

**Upstream status:** `candidate`

### Files

The patch edits these files and the tests next to them:

- `pkg/api/v1alpha1/workerpool_types.go`
- `pkg/api/v1alpha1/zz_generated.deepcopy.go`
- `manifests/ate-install/generated/ate.dev_workerpools.yaml`
- `cmd/atecontroller/internal/controllers/workerpool_apply.go`
- `cmd/ateom-microvm/main.go`
- `cmd/ateom-microvm/run.go`
- `cmd/ateom-microvm/internal/kata/specconv.go`
- `internal/ocispec/microvm.go`
- `docs/api-guide.md`

### Reason

A micro-VM actor must mount a FUSE filesystem, for example LayerFS, in its guest. The guest container has no `/dev/fuse`, and the actor cannot `mknod` it.

The operator turns the device on with a WorkerPool field. The micro-VM worker adds the node to the OCI spec that it builds. No plugin or configuration can change that code, so rungs 1 to 4 of Principle II do not apply.

### Removal condition

Remove the patch when upstream merges it. Also remove it if upstream adds a different device API for micro-VM actors.
