# Spike: LayerFS for the factory manager home

> **Status:** Spike report, 2026-11-09. Research only. It changes no code, no
> cluster state and no task.
> Branch `spike/layerfs-persistence`, base `main` at `ad3ce36d`.

## Question

Can Substrate LayerFS hold the home of the factory manager task (`f-manager`)?
The home has about 1.5 GB of files and a Claude transcript of about 100 MB.
The manager changes it every few seconds. The home must survive these events:

- A task delete.
- An image change.
- A node loss.

Can LayerFS replace or back up the current git home backup
(claude-factory `docs/remote-runner.md`, section "Back up the manager home")?

## Recommendation: no

Do not put the manager home on LayerFS. Do not use LayerFS as its backup.
These are the reasons:

1. **LayerFS adds no durability.** The Store is one local SQLite file. It has
   these properties:
   - `synchronous = OFF`
   - `journal_mode = MEMORY`
   - no `fsync`

   LayerFS runs only in its Disposable profile. The LayerFS README says: do
   not use it "as the only copy of data you cannot rebuild". LayerFS has no
   remote or object-store tier ("cloud placement is later scope"). The home
   survives a delete or a node loss only if something copies the bytes off the
   node. LayerFS does not do that.
2. **An off-node copy of a Store costs more than git.** The Store is one file,
   with no WAL. A tool such as Litestream cannot stream its changes. Each
   off-node copy is a full copy of a file of 1.6 GB or more. The file only
   grows: LayerFS never deletes objects.
3. **The manager cannot mount it today.** The manager runs on gVisor (AX's
   default class). Substrate gives `/dev/fuse` only to micro-VM pools, and AX
   cannot select such a pool (bead `substrate-any`).
4. **The real gap is in AX, not in the file system.** The 2026-11-09 loss came
   from `ax delete task`, which deletes the actor and its snapshot. Substrate
   Tags already keep a snapshot past a delete. AX must use them (bead `ax-oox`).
   But see the next section: `ax-oox` as written does not work for an image change.

Do this instead:

- Keep the git backup. It is the only off-node copy that runs while the
  manager runs. It runs at the interval set by `FACTORY_AX_BACKUP_EVERY`
  (default 600000 ms).
- Fix `ax-oox` for the template rule below. Then build it. The fixed `ax-oox`
  gives a byte-equal `/workspace` across a delete, an image change and a node
  loss, at the point of the last tag.
- Look at LayerFS again only if the factory needs its other features (a
  Commit for each tool call, Branches, rewind). Do not look at it for
  durability.

## Finding: a Tag cannot seed a task with a new image

Substrate refuses `CreateActor` with a `source_tag` from another
ActorTemplate:

```text
cmd/ateapi/internal/controlapi/actor.go:194
  if tag.GetStatus().GetActorTemplateUid() != template.GetMetadata().GetUid() {
    return nil, apierror.FailedPrecondition("source Tag must be taken from an actor with ActorTemplate uid %q", ...)
```

AX makes one ActorTemplate for each combination of image and env.
`taskTemplateName` in `internal/controller/reconciler.go:437` (stigenai/ax
`29ab570`) hashes the image and all extra env.

The extra env holds `AX_TASK_YAML`. That variable holds the whole Task spec. So a new image gives a
new template with a new UID. A new `FACTORY_AX_LAUNCH_ID` also gives a new
template. `factory manager --ax` sets a new one on each launch. Step 2 of
`ax-oox` ("the new task, a new image is allowed, starts with that /workspace")
fails with `FailedPrecondition`.

Substrate has a path that works. A client can repoint an actor to another
template while it is `SUSPENDED` (`UpdateActor`). A resume that finds a different
template "restores the durable data only and boots the guest fresh"
(`docs/api-guide.md`, Snapshot Storage Layout). AX never calls `UpdateActor`
today. These are the options for `ax-oox`:

| Option | Where | Covers |
| --- | --- | --- |
| A. Create the new actor from the tag under the **old** template, then repoint it to the new template before the first resume | AX only | delete, image change, node loss |
| B. Change the image of a suspended task in place: repoint its actor, keep the actor | AX only | image change with no delete |
| C. Let `CreateActor` take a tag from another template with a data-only restore | Substrate | delete, image change, node loss |

Option A needs no Substrate change. The old template must still exist.
Option B makes a delete unnecessary for an image change. It does not help after
a delete or a node loss.

## What LayerFS is

Source: `stigenai/layerfs` (fork of `Ephemeral-AI-Lab/layerfs`), `main` at
`67310175e`, release 1.1.0. Earlier AX spike: branch `spike/ax-deploy`,
`docs/ax-spike.md` (bead `claude-factory-wzi7`).

- **Store.** One SQLite file holds every LayerStack, Layer, Branch, Commit and
  content object. The same file stores the pack bodies as BLOBs. The Store has
  no server.
- **Content.** LayerFS stores content as content-defined chunks, rope extents
  and tree objects. A small edit of a big file stores only the changed chunks.
  Objects are append-only. Normal operations never delete an object. So there
  is no garbage collection.
- **Versions.** A Commit is an immutable snapshot of a Branch. A Layer is a
  published snapshot. The same Store file keeps the history.
- **Workspace.** A Workspace is an ephemeral, writable view of one Branch head.
  LayerFS serves it over FUSE, or materializes it to a directory. Writes go to
  an overlay. A Commit saves them. Unmount deletes the overlay, not the history.
- **Model.** Each tool call mounts, runs, commits and unmounts. The design does
  not cover a process that holds the mount for days and commits while it runs.
- **FUSE limits.** The view omits FIFOs, sockets, device nodes, set-id bits
  and separate atime and ctime.
- **Placement.** Every opener of a Store must share one Linux kernel. The Store
  must sit on a local ext4, xfs or btrfs file system (from `docs/ax-spike.md`).
  A Store on virtiofs, which VMs share, does not meet that rule.

## Durability

| Event | LayerFS Store in tmpfs | LayerFS Store in the DurableDir | DurableDir today | DurableDir + Tags (`ax-oox`, fixed) | Git backup |
| --- | --- | --- | --- | --- | --- |
| Suspend and resume | lost | kept | kept | kept | kept |
| `ax delete task` | lost | lost | lost | kept, at the last tag | kept, RPO 10 min |
| Image change | lost | lost (needs a delete) | lost (needs a delete) | kept, at the last tag | kept, RPO 10 min |
| Node loss while running | lost | at the last suspend | at the last suspend | at the last tag | kept, RPO 10 min |
| Process crash | Store can be corrupt (`synchronous = OFF`) | same | n/a | n/a | n/a |

Notes:

- If a node is lost, the actor moves to `ACTOR_STATE_CRASHED`. `RevertActor`
  brings it back at its last external snapshot. Substrate writes that snapshot
  only on a suspend. The manager runs all the time. So its last snapshot can be
  old.
- On an eviction, the actor gets 30 minutes to suspend. A hard node loss
  gives no time.
- A Tag copies the snapshot to its own prefix in object storage
  (`<location>/atespaces/<atespace>/tags/<tag uid>`). A delete of the actor
  does not touch it.

## Cost at this size

The numbers come from the runs below. One sample each. The 1.6 GB values are
estimates from them, not measurements.

| Operation | Measured | Estimate for the home (1.6 GB) |
| --- | --- | --- |
| LayerFS Project Init (first seal) | 295 MB in 31.2 s on 1 vCPU (AX micro-VM); 3.99 GB in 36.2 s on 18 CPUs | about 2 to 3 min on 1 vCPU |
| LayerFS Commit | 5.21 GB captured in 4.9 s (fsstress, 8 CPUs) | in proportion to the dirty bytes, not to the home size |
| LayerFS metadata work in the mount | 5.9 times slower than a plain tree (`go list`) | the manager pays this on every file call |
| LayerFS off-node copy | Store 196 MB, tmpfs to virtiofs, 2.36 s | full file each time, 1.6 GB or more, and it grows |
| LayerFS mount in a micro-VM (kind, `layerfs-e2e` run `42456674447`) | daemon start 0.03 s, mount 0.005 s, warm mount 0.024 s | small |
| Substrate snapshot (suspend) | not measured | full tar of each DurableDir volume, then upload. Not incremental |
| Substrate Tag | not measured | server-side copy of the snapshot objects |
| Git backup | not measured | changed files only. A file over 50 MB goes as gzip, so each push stores a new gzip of the transcript |

Sources: `docs/ax-spike.md` on `spike/ax-deploy`, the LayerFS README results
table, and the job log of `layerfs-e2e` run `42456674447` on
`feat/microvm-fuse-device`.

## Can a gVisor AX task mount LayerFS?

Not with what Substrate and AX have today.

- The WorkerPool CRD refuses `guestDevices` on a gVisor pool
  (`ate.dev_workerpools.yaml`, the `guestDevices` rule). `ateom-gvisor` has no
  `--guest-devices` flag (`workerpool_apply.go:143`).
- `internal/ocispec/gvisor.go` adds no device to the OCI spec.
- gVisor has its own FUSE in the Sentry. This spike did not check if the
  runsc build and flags that Substrate uses expose `/dev/fuse`. See open
  question 1.

What a mount needs, on the micro-VM path that works today:

1. Substrate: a WorkerPool with `sandboxClass: microvm` and
   `guestDevices: [fuse]`. This is fork-only (stigenai/substrate `main`, not upstream).
2. AX: a Task field that selects that pool (bead `substrate-any`).
3. AX: `SYS_ADMIN`, `SETUID` and `SETGID` in `spec.capabilities`
   (`FACTORY_AX_CAPABILITIES`).
4. claude-factory: run the manager as `sandboxClass: microvm`. Start
   `layerfs-daemon`, mount the home, commit on a timer, and copy the Store
   off the node. None of this exists.

## Open questions

1. Does the gVisor (runsc) build in Substrate expose `/dev/fuse` to a sandbox?
   If it does, what flag and which OCI device entry does it need? Answer it
   only if LayerFS is chosen for a gVisor workload later.
2. Can a LayerFS Workspace stay mounted for days and commit while processes
   in it run? The daemon model is one execution for each mount.
3. Does the manager home hold sockets, FIFOs or set-id files? The LayerFS
   FUSE view omits them. The herdr and SSH attach files are candidates.
4. How fast does the git backup repository grow? Each push stores a new gzip
   of the 100 MB transcript. Measure the growth for one day on
   `stigenai/factory-manager-backup`.
5. How long does a suspend of the manager take with a 1.6 GB DurableDir? That
   sets how often `ax-oox` can tag without a long pause of the manager.
6. For `ax-oox`: option A, B or C (above)? Option A and B are AX only.

## Method

- Read `internal/e2e/manual/layerfs` and `.github/workflows/layerfs-e2e.yaml`
  on this branch, and the job log of run `42456674447`.
- Read the LayerFS source and docs: `README.md`, `docs/general/concepts.md`,
  `core/crates/layerfs-storage/README.md`,
  `engine/crates/layerfs-persistence/README.md`, `AGENTS.md`, and
  `docs/ax-spike.md` on `spike/ax-deploy`.
- Read the Substrate API: `pkg/proto/ateapipb/ateapi.proto` (Tags,
  `RevertActor`, `DurableDirVolumeSource`), `docs/api-guide.md`,
  `cmd/ateapi/internal/controlapi/actor.go`, `cmd/ateom-gvisor/durable.go`,
  `cmd/atecontroller/internal/controllers/workerpool_apply.go`.
- Read AX (`stigenai/ax` `29ab570`): `internal/substrate/client.go`,
  `internal/controller/reconciler.go`, and bead `ax-oox`.
- Read claude-factory (`db2d003c`): `docs/remote-runner.md` and `src/ax.ts`.
- This spike ran nothing on a cluster and did not touch `f-manager`.
