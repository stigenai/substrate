//  Copyright 2026 The Agent Substrate Authors
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/atelet/internal/ateletpath"
	"go.ate.dev/substrate/cmd/atelet/internal/trustbundle"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/supervisorbootstrap"
	"go.ate.dev/substrate/internal/volumepath"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// supervisorMintTimeout bounds one MintSupervisorBootstrap call.
	supervisorMintTimeout = 10 * time.Second

	protectedDirMode  os.FileMode = 0o700
	protectedFileMode os.FileMode = 0o600
)

// systemInfoVolumesFor lists spec's system-info volumes with their host roots.
func systemInfoVolumesFor(actorUID string, spec *ateletpb.WorkloadSpec) []*systemInfoVolume {
	var volumes []*systemInfoVolume
	for _, vol := range spec.GetVolumes() {
		if si := vol.GetSystemInfo(); si != nil {
			volumes = append(volumes, &systemInfoVolume{
				Name: vol.GetName(),
				Root: ateletpath.SystemInfoVolumeRoot(actorUID, vol.GetName()),
				Spec: si,
			})
		}
	}
	return volumes
}

type registeredActor struct {
	uid string
	ref resources.ActorRef

	// mu covers the volumes' file writes and stale.
	mu      sync.Mutex
	stale   bool
	volumes []*systemInfoVolume
}

// systemInfoVolumeRefresher writes system-info volumes when an actor starts
// and refreshes them as needed.
type systemInfoVolumeRefresher struct {
	bundles   *trustbundle.Source
	hasSynced cache.InformerSynced

	// queue carries bundle names from informer events to the run loop.
	queue workqueue.TypedRateLimitingInterface[string]

	// mu covers actors
	mu sync.Mutex
	// TODO(#1372): in memory only; an atelet restart drops the registry until
	// each actor's next Run/Restore.
	actors map[string]*registeredActor

	// bootstrap issues the credential for a supervisor bootstrap projection.
	// Nil leaves such a projection unwritable.
	bootstrap ateapi.WorkerServiceClient
}

// refresherOption configures a systemInfoVolumeRefresher.
type refresherOption func(*systemInfoVolumeRefresher)

// withSupervisorBootstrap sets the client that issues supervisor bootstrap
// credentials.
func withSupervisorBootstrap(c ateapi.WorkerServiceClient) refresherOption {
	return func(r *systemInfoVolumeRefresher) { r.bootstrap = c }
}

// newSystemInfoVolumeRefresher subscribes to ClusterTrustBundle events.
func newSystemInfoVolumeRefresher(bundles *trustbundle.Source, informer cache.SharedIndexInformer, opts ...refresherOption) *systemInfoVolumeRefresher {
	r := &systemInfoVolumeRefresher{
		bundles: bundles,
		queue:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		actors:  map[string]*registeredActor{},
	}
	for _, opt := range opts {
		opt(r)
	}
	if informer != nil {
		informer.AddEventHandler(r.eventHandler())
		r.hasSynced = informer.HasSynced
	}
	return r
}

// Register records actorUID's system-info volumes and writes their contents
// from current cluster state. If actorUID is already registered (for example
// after a worker pod crash left a stale entry without Terminate), the previous
// registration is superseded, even if this registration's initial write fails.
// The failed registration is removed without restoring the previous one.
func (r *systemInfoVolumeRefresher) Register(ctx context.Context, actorUID string, ref resources.ActorRef, volumes []*systemInfoVolume) (*registeredActor, error) {
	actor := &registeredActor{uid: actorUID, ref: ref, volumes: volumes}
	// Held until the initial write finishes so a refresh cannot interleave.
	actor.mu.Lock()
	defer actor.mu.Unlock()

	r.mu.Lock()
	prev := r.actors[actorUID]
	r.actors[actorUID] = actor
	r.mu.Unlock()
	if prev != nil {
		prev.mu.Lock()
		prev.stale = true
		prev.mu.Unlock()
		slog.Info("Superseded a stale system-info volume registration",
			slog.String("actor_uid", actorUID),
			slog.Any("actor", ref))
	}

	for _, v := range volumes {
		if err := r.write(ctx, ref, actorUID, v); err != nil {
			r.mu.Lock()
			if r.actors[actorUID] == actor {
				delete(r.actors, actorUID)
				actor.stale = true
			}
			r.mu.Unlock()
			return nil, fmt.Errorf("while populating system-info volume %q: %w", v.Name, err)
		}
	}
	return actor, nil
}

// Deregister drops actorUID's registration. After Deregister returns, no more
// system-info volumes will be written for the actor.
func (r *systemInfoVolumeRefresher) Deregister(actorUID string) {
	r.mu.Lock()
	actor := r.actors[actorUID]
	delete(r.actors, actorUID)
	r.mu.Unlock()
	if actor != nil {
		// Setting stale stops a refresh that snapshotted the entry before
		// the delete.
		actor.mu.Lock()
		actor.stale = true
		actor.mu.Unlock()
	}
}

// DeregisterOwned removes owner only when its UID still points at it. This
// protects a newer registration from cleanup belonging to an older operation.
func (r *systemInfoVolumeRefresher) DeregisterOwned(owner *registeredActor) {
	if owner == nil {
		return
	}
	r.mu.Lock()
	if r.actors[owner.uid] != owner {
		r.mu.Unlock()
		return
	}
	delete(r.actors, owner.uid)
	r.mu.Unlock()

	owner.mu.Lock()
	owner.stale = true
	owner.mu.Unlock()
}

// collectData builds the volume's contents keyed by volume-relative path.
func (r *systemInfoVolumeRefresher) collectData(ctx context.Context, ref resources.ActorRef, actorUID string, si *ateletpb.SystemInfoVolumeSource) (payload map[string][]byte, err error) {
	payload = map[string][]byte{}
	for _, dataSource := range si.GetDataSources() {
		switch {
		case dataSource.GetSupervisorBootstrap() != nil:
			b := dataSource.GetSupervisorBootstrap()
			if len(si.GetDataSources()) != 1 || b.GetProfile() != supervisorbootstrap.Profile {
				return nil, fmt.Errorf("invalid protected supervisor projection")
			}
			// The golden actor, and any actor with no activation, gets an
			// empty volume: it must never hold a credential.
			if b.GetActivationId() == "" {
				continue
			}
			if r.bootstrap == nil || b.GetWorkerUid() == "" {
				return nil, fmt.Errorf("protected supervisor issuer unavailable")
			}
			mintCtx, cancel := context.WithTimeout(ctx, supervisorMintTimeout)
			resp, err := r.bootstrap.MintSupervisorBootstrap(mintCtx, &ateapi.MintSupervisorBootstrapRequest{
				Actor:        ref.ToObjectRef(),
				ActorUid:     actorUID,
				ActivationId: b.GetActivationId(),
				WorkerUid:    b.GetWorkerUid(),
			})
			cancel()
			if err != nil || resp.GetToken() == "" {
				return nil, fmt.Errorf("protected supervisor issuance failed")
			}
			payload[supervisorbootstrap.FileName] = []byte(resp.GetToken())
		case dataSource.GetTrustBundle() != nil:
			tb := dataSource.GetTrustBundle()
			pemBundle, err := r.bundles.Combined(tb.GetNames())
			if err != nil {
				return nil, fmt.Errorf("system-info projection %q: %w", tb.GetPath(), err)
			}
			payload[tb.GetPath()] = pemBundle
		case dataSource.GetActorMetadata() != nil:
			for _, item := range dataSource.GetActorMetadata().GetItems() {
				var value string
				switch item.GetField() {
				case ateletpb.ActorMetadataField_ACTOR_METADATA_FIELD_NAME:
					value = ref.Name
				case ateletpb.ActorMetadataField_ACTOR_METADATA_FIELD_ATESPACE:
					value = ref.Atespace
				case ateletpb.ActorMetadataField_ACTOR_METADATA_FIELD_UID:
					value = actorUID
				default:
					// Unknown fields come only from a newer ateapi; skip the
					// item rather than write an empty file under its path.
					continue
				}
				payload[item.GetPath()] = []byte(value)
			}
		}
	}
	return payload, nil
}

// write brings the volume's files up to date.
func (r *systemInfoVolumeRefresher) write(ctx context.Context, ref resources.ActorRef, actorUID string, v *systemInfoVolume) error {
	payload, err := r.collectData(ctx, ref, actorUID, v.Spec)
	if err != nil {
		return fmt.Errorf("while collecting volume contents: %w", err)
	}
	_, err = v.apply(payload)
	return err
}

// systemInfoVolume is one system-info volume of an actor and its host root.
type systemInfoVolume struct {
	Name string
	Root string
	Spec *ateletpb.SystemInfoVolumeSource
}

// apply writes payload into the volume and reports whether any file changed.
func (v *systemInfoVolume) apply(payload map[string][]byte) (changed bool, err error) {
	dirMode, fileMode := v.modes()
	if err := os.MkdirAll(v.Root, dirMode); err != nil {
		return false, fmt.Errorf("while creating %q: %w", v.Root, err)
	}
	if dirMode == protectedDirMode {
		// MkdirAll leaves the mode of an existing directory alone.
		if err := os.Chmod(v.Root, dirMode); err != nil {
			return false, fmt.Errorf("while restricting %q: %w", v.Root, err)
		}
	}
	root, err := os.OpenRoot(v.Root)
	if err != nil {
		return false, fmt.Errorf("while opening %q: %w", v.Root, err)
	}
	defer root.Close()
	for _, relPath := range slices.Sorted(maps.Keys(payload)) {
		written, err := writeSystemInfoFile(root, relPath, payload[relPath], fileMode)
		if err != nil {
			return changed, err
		}
		changed = changed || written
	}
	return changed, nil
}

// modes returns the directory and file modes of the volume. A supervisor
// bootstrap volume is readable by root only.
func (v *systemInfoVolume) modes() (dirMode, fileMode os.FileMode) {
	for _, ds := range v.Spec.GetDataSources() {
		if ds.GetSupervisorBootstrap() != nil {
			return protectedDirMode, protectedFileMode
		}
	}
	return 0o755, 0o644
}

// writeSystemInfoFile writes one projected file inside root with mode, skipping
// it if the contents already match, and reports whether it wrote. Also
// re-validates relPath.
func writeSystemInfoFile(root *os.Root, relPath string, data []byte, mode os.FileMode) (bool, error) {
	if err := volumepath.ValidateProjected(relPath); err != nil {
		return false, fmt.Errorf("invalid system-info path %q: %w", relPath, err)
	}
	dst := filepath.FromSlash(relPath)
	if existing, err := root.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
		if mode == protectedFileMode {
			// Matching contents can carry a looser mode from an earlier write.
			if err := root.Chmod(dst, mode); err != nil {
				return false, fmt.Errorf("while restricting system-info file %q under %q: %w", relPath, root.Name(), err)
			}
		}
		return false, nil
	}
	if dir := filepath.Dir(dst); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("while creating parent of %q under %q: %w", relPath, root.Name(), err)
		}
	}
	if err := writeFileAtomicRoot(root, dst, data, mode); err != nil {
		return false, fmt.Errorf("while writing system-info file %q under %q: %w", relPath, root.Name(), err)
	}
	return true, nil
}

// writeFileAtomicRoot is atomicfile.Write confined to root. The fixed temp
// name is safe because writers of an actor's volumes serialize on its lock.
func writeFileAtomicRoot(root *os.Root, relPath string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Dir(relPath), filepath.Base(relPath)
	tmp := filepath.Join(dir, "."+base+".tmp")
	f, err := root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, relPath)
}

// eventHandler enqueues the bundle names an event touches.
func (r *systemInfoVolumeRefresher) eventHandler() cache.ResourceEventHandler {
	enqueue := func(obj any) {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = d.Obj
		}
		ctb, err := meta.Accessor(obj)
		if err != nil {
			return
		}

		// Map clustertrustbundles to the particular Substrate trust bundle names they back.
		switch ctb.GetName() {
		case trustbundle.EgressCTB:
			r.queue.Add(trustbundle.EgressName)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj any) { enqueue(obj) },
		DeleteFunc: enqueue,
	}
}

// run drains the queue until ctx ends.
func (r *systemInfoVolumeRefresher) run(ctx context.Context) {
	defer r.queue.ShutDown()
	if r.hasSynced != nil && !cache.WaitForCacheSync(ctx.Done(), r.hasSynced) {
		return
	}
	go wait.UntilWithContext(ctx, r.runWorker, time.Second)
	<-ctx.Done()
}

func (r *systemInfoVolumeRefresher) runWorker(ctx context.Context) {
	for r.processNextWorkItem(ctx) {
	}
}

func (r *systemInfoVolumeRefresher) processNextWorkItem(ctx context.Context) bool {
	bundleName, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(bundleName)

	if err := r.refreshBundle(ctx, bundleName); err != nil {
		r.queue.AddRateLimited(bundleName)
		return true
	}
	r.queue.Forget(bundleName)
	return true
}

// refreshBundle rewrites every registered volume that projects bundleName and
// is behind its current contents.
func (r *systemInfoVolumeRefresher) refreshBundle(ctx context.Context, bundleName string) error {
	targets := r.projecting(bundleName)
	if len(targets) == 0 {
		return nil
	}
	var writeErr error
	refreshed := 0
	for _, actor := range targets {
		actor.mu.Lock()
		if actor.stale {
			actor.mu.Unlock()
			continue
		}
		for _, v := range actor.volumes {
			if !projectsBundle(v.Spec, bundleName) {
				continue
			}
			payload, err := r.collectData(ctx, actor.ref, actor.uid, v.Spec)
			if err != nil {
				// Not requeued: the informer delivers the next change to
				// the bundle, which is what could make it resolvable.
				slog.WarnContext(ctx, "Trust bundle unresolvable; projected files keep their last contents", slog.String("actor_uid", actor.uid), slog.String("volume", v.Name), slog.String("bundle", bundleName), slog.Any("err", err))
				continue
			}
			changed, err := v.apply(payload)
			if err != nil {
				slog.ErrorContext(ctx, "Failed to refresh system-info volume", slog.String("actor_uid", actor.uid), slog.String("volume", v.Name), slog.String("bundle", bundleName), slog.Any("err", err))
				writeErr = err
				continue
			}
			if changed {
				refreshed++
			}
		}
		actor.mu.Unlock()
	}
	if refreshed > 0 {
		slog.InfoContext(ctx, "Refreshed projected trust bundle under running actors", slog.String("bundle", bundleName), slog.Int("volumes", refreshed))
	}
	return writeErr
}

// projectsBundle reports whether the volume spec projects the named bundle.
func projectsBundle(si *ateletpb.SystemInfoVolumeSource, bundleName string) bool {
	for _, ds := range si.GetDataSources() {
		if tb := ds.GetTrustBundle(); tb != nil && slices.Contains(tb.GetNames(), bundleName) {
			return true
		}
	}
	return false
}

// projecting snapshots the actors with a volume projecting bundleName.
func (r *systemInfoVolumeRefresher) projecting(bundleName string) []*registeredActor {
	r.mu.Lock()
	defer r.mu.Unlock()
	var targets []*registeredActor
	for _, actor := range r.actors {
		for _, v := range actor.volumes {
			if projectsBundle(v.Spec, bundleName) {
				targets = append(targets, actor)
				break
			}
		}
	}
	return targets
}

// validateSupervisorTarget refuses a spec whose supervisor activation names a
// worker other than the one the request targets, before Run or Restore can
// project a token for another worker. An empty activation projects nothing.
func validateSupervisorTarget(spec *ateletpb.WorkloadSpec, workerPodUID string) error {
	for _, ds := range supervisorDataSources(spec) {
		if ds.GetProfile() != supervisorbootstrap.Profile {
			return apierror.InvalidArgument("unsupported supervisor profile")
		}
		if (ds.GetActivationId() == "") != (ds.GetWorkerUid() == "") || (ds.GetWorkerUid() != "" && ds.GetWorkerUid() != workerPodUID) {
			return apierror.InvalidArgument("supervisor target differs from the assigned worker")
		}
	}
	return nil
}

// validateSupervisorSnapshot refuses to capture or restore guest memory for a
// spec with a supervisor projection: that memory can hold the token.
func validateSupervisorSnapshot(spec *ateletpb.WorkloadSpec, snap *ateletpb.Snapshot) error {
	if len(supervisorDataSources(spec)) > 0 && snap.GetFidelity() == ateletpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		return apierror.InvalidArgument("protected supervisor snapshots must exclude guest memory")
	}
	return nil
}

func supervisorDataSources(spec *ateletpb.WorkloadSpec) []*ateletpb.SupervisorBootstrapDataSource {
	var sources []*ateletpb.SupervisorBootstrapDataSource
	for _, v := range spec.GetVolumes() {
		for _, ds := range v.GetSystemInfo().GetDataSources() {
			if b := ds.GetSupervisorBootstrap(); b != nil {
				sources = append(sources, b)
			}
		}
	}
	return sources
}
