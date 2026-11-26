// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controlapi

import (
	"context"
	"testing"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/scheduling"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/cmd/ateapi/internal/store/storetest"
	"go.ate.dev/substrate/cmd/ateapi/internal/workercache"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/supervisorbootstrap/supervisorbootstraptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

func TestSupervisorActivationDurableBeforeWorkloadAndRotates(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	template := supervisorbootstraptest.Template()
	name := testWorkerUID("supervisor-worker")
	// Room for two: the worker cache still books the first assignment when the
	// actor resumes again.
	worker := &ateapi.Worker{
		Metadata: &ateapi.ResourceMetadata{Name: name}, WorkerNamespace: "worker-ns", WorkerPool: "pool", WorkerPod: "supervisor-worker",
		WorkerPodUid: name, NodeName: "node-1",
		Status: &ateapi.WorkerStatus{State: ateapi.WorkerState_WORKER_STATE_ACTIVE, RegisteredEpoch: 1, Capacity: &ateapi.WorkerResources{Actors: 2}, SandboxClasses: workerSandboxClasses(testSandboxRuntime())},
	}
	if _, err := persistence.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	ref := resources.ActorRef{Atespace: "team-a", Name: "supervisor"}
	actor := storetest.MustCreateActor(t, ctx, persistence, &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: ref.Atespace, Name: ref.Name},
		Status:   &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	})
	cacheCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wc := workercache.New(persistence, time.Minute)
	if err := wc.Start(cacheCtx); err != nil {
		t.Fatal(err)
	}
	w := &ActorWorkflow{store: persistence, workerCache: wc, scheduler: scheduling.New(wc)}

	assigned, _, err := w.assignWorkerAttempt(ctx, ref, actor, template)
	if err != nil {
		t.Fatal(err)
	}
	activation := assigned.GetStatus().GetSupervisorActivation()
	if activation.GetActivationId() == "" || activation.GetWorkerUid() != worker.WorkerPodUid {
		t.Fatalf("activation not bound to the worker: %v", activation)
	}
	persisted, err := persistence.GetActor(ctx, ref)
	if err != nil || !proto.Equal(persisted.GetStatus().GetSupervisorActivation(), activation) {
		t.Fatal("activation not durable before workload")
	}
	spec, err := workloadSpecFromActorTemplate(template, persisted, nil)
	if err != nil {
		t.Fatal(err)
	}
	projected := spec.Volumes[0].GetSystemInfo().DataSources[0].GetSupervisorBootstrap()
	if projected.GetActivationId() != activation.ActivationId || projected.GetWorkerUid() != activation.WorkerUid {
		t.Fatal("wire differs from retained activation")
	}

	// Finish the suspend, which frees the worker, then resume again.
	suspended, err := persistence.UpdateActorAndRelease(ctx, ref, store.PreconditionFrom(persisted), func(a *ateapi.Actor) error {
		a.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
		a.Status.WorkerAssignment = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resumed, _, err := w.assignWorkerAttempt(ctx, ref, suspended, template)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.GetStatus().GetSupervisorActivation().GetActivationId() == activation.ActivationId {
		t.Fatal("new activation reused the prior identity")
	}
	if resumed.GetMetadata().GetUid() != actor.GetMetadata().GetUid() {
		t.Fatal("test must keep the actor incarnation")
	}
}

func TestSupervisorGoldenActorGetsNoActivation(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	template := supervisorbootstraptest.Template()
	name := testWorkerUID("golden-worker")
	if _, err := persistence.CreateWorker(ctx, &ateapi.Worker{
		Metadata: &ateapi.ResourceMetadata{Name: name}, WorkerNamespace: "worker-ns", WorkerPool: "pool", WorkerPod: "golden-worker",
		WorkerPodUid: name, NodeName: "node-1",
		Status: &ateapi.WorkerStatus{State: ateapi.WorkerState_WORKER_STATE_ACTIVE, RegisteredEpoch: 1, Capacity: &ateapi.WorkerResources{Actors: 1}, SandboxClasses: workerSandboxClasses(testSandboxRuntime())},
	}); err != nil {
		t.Fatal(err)
	}
	// The template reconciler names the golden actor after the template UID.
	ref := resources.ActorRef{Atespace: template.GetMetadata().GetAtespace(), Name: template.GetMetadata().GetUid()}
	actor := storetest.MustCreateActor(t, ctx, persistence, &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: ref.Atespace, Name: ref.Name},
		Status:   &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	})
	cacheCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wc := workercache.New(persistence, time.Minute)
	if err := wc.Start(cacheCtx); err != nil {
		t.Fatal(err)
	}
	w := &ActorWorkflow{store: persistence, workerCache: wc, scheduler: scheduling.New(wc)}
	assigned, _, err := w.assignWorkerAttempt(ctx, ref, actor, template)
	if err != nil {
		t.Fatal(err)
	}
	if assigned.GetStatus().GetSupervisorActivation() != nil {
		t.Fatal("golden actor got an activation")
	}
	spec, err := workloadSpecFromActorTemplate(template, assigned, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := spec.Volumes[0].GetSystemInfo().DataSources[0].GetSupervisorBootstrap(); b.GetActivationId() != "" || b.GetWorkerUid() != "" {
		t.Fatal("golden workload carried an activation")
	}
}

func TestSupervisorWorkloadNeedsActivation(t *testing.T) {
	template := supervisorbootstraptest.Template()
	actor := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: "team-a", Name: "ordinary"}}
	if _, err := workloadSpecFromActorTemplate(template, actor, nil); err == nil {
		t.Fatal("protected actor without an activation admitted")
	}
}

func TestValidateSupervisorRestore(t *testing.T) {
	template := supervisorbootstraptest.Template()
	snapshot := func(fidelity ateapi.SnapshotFidelity, templateUID string) *ateapi.Snapshot {
		return &ateapi.Snapshot{Uuid: "snap-1", Generation: 1, Owner: ateapi.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, Fidelity: fidelity, ActorTemplateUid: templateUID, Locality: "node-1"}
	}
	for name, tc := range map[string]struct {
		snaps   []*ateapi.Snapshot
		tmpl    *ateapi.ActorTemplate
		wantErr bool
	}{
		"cold boot":                      {tmpl: template},
		"rootfs snapshot":                {snaps: []*ateapi.Snapshot{snapshot(ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS, template.Metadata.Uid)}, tmpl: template},
		"memory snapshot":                {snaps: []*ateapi.Snapshot{snapshot(ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, template.Metadata.Uid)}, tmpl: template, wantErr: true},
		"unstamped memory snapshot":      {snaps: []*ateapi.Snapshot{snapshot(ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "")}, tmpl: template, wantErr: true},
		"memory under replaced template": {snaps: []*ateapi.Snapshot{snapshot(ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "00000000-0000-4000-8000-000000000000")}, tmpl: template},
		"ordinary template":              {snaps: []*ateapi.Snapshot{snapshot(ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "")}, tmpl: &ateapi.ActorTemplate{}},
	} {
		t.Run(name, func(t *testing.T) {
			actor := &ateapi.Actor{Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED, Snapshots: tc.snaps}}
			err := validateSupervisorRestore(actor, tc.tmpl)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("validateSupervisorRestore() = %v, want error %t", err, tc.wantErr)
			}
			if err != nil && apierror.Code(err) != codes.FailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition", apierror.Code(err))
			}
		})
	}
}
