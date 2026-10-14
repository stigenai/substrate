// Copyright 2026 Google LLC
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
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/scheduling"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store/storetest"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/workercache"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap/supervisorbootstraptest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestSupervisorActivationDurableBeforeWorkloadAndRotates(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	template := supervisorbootstraptest.Template()
	template.SandboxConfig = &ateapipb.SandboxConfig{SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR}
	template, err := persistence.CreateActorTemplate(ctx, template)
	if err != nil {
		t.Fatal(err)
	}
	worker := &ateapipb.Worker{Metadata: &ateapipb.ResourceMetadata{Name: testWorkerUID("supervisor-worker")}, WorkerNamespace: "worker-ns", WorkerPool: "pool", WorkerPod: "supervisor-worker", WorkerPodUid: testWorkerUID("supervisor-worker"), NodeName: "node-1", SandboxClass: "gvisor", Status: &ateapipb.WorkerStatus{State: ateapipb.WorkerState_WORKER_STATE_ACTIVE, Capacity: &ateapipb.WorkerResources{Actors: 1}}}
	if _, err := persistence.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	actor := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "supervisor"}, ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "template-1"}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}})
	cacheCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wc := workercache.New(persistence, time.Minute)
	if err := wc.Start(cacheCtx); err != nil {
		t.Fatal(err)
	}
	w := &ActorWorkflow{store: persistence, workerCache: wc, scheduler: scheduling.New(wc)}
	ref := resources.ActorRef{Atespace: "team-a", Name: "supervisor"}
	leaseCtx, lease, err := w.acquireActorLease(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	assigned, _, err := w.ensureWorkerAssigned(leaseCtx, ref, actor, template)
	if err != nil {
		t.Fatal(err)
	}
	activation := assigned.Status.GetSupervisorActivation()
	if activation.GetActivationId() == "" || activation.GetWorkerUid() != worker.WorkerPodUid {
		t.Fatal("activation not bound")
	}
	persisted, err := persistence.GetActor(ctx, ref)
	if err != nil || !proto.Equal(persisted.Status.GetSupervisorActivation(), activation) {
		t.Fatal("activation not durable before workload")
	}
	spec, err := workloadSpecFromActorTemplate(template, persisted)
	if err != nil {
		t.Fatal(err)
	}
	projected := spec.Volumes[0].GetSystemInfo().DataSources[0].GetSupervisorBootstrap()
	if projected.GetActivationId() != activation.ActivationId || projected.GetWorkerUid() != activation.WorkerUid {
		t.Fatal("wire differs from retained activation")
	}
	retry, _, err := w.ensureWorkerAssigned(leaseCtx, ref, persisted, template)
	if err != nil || !proto.Equal(retry.Status.GetSupervisorActivation(), activation) {
		t.Fatalf("same resume changed activation: %v", err)
	}
	// Simulate the completed pause boundary under the same real lifecycle lease.
	paused, err := persistence.UpdateActor(ctx, ref, store.PreconditionFrom(retry), func(a *ateapipb.Actor) error { a.Status.State = ateapipb.ActorState_ACTOR_STATE_PAUSED; return nil })
	if err != nil {
		t.Fatal(err)
	}
	resumed, _, err := w.ensureWorkerAssigned(leaseCtx, ref, paused, template)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status.GetSupervisorActivation().GetActivationId() == activation.ActivationId {
		t.Fatal("new activation reused prior generation")
	}
	if resumed.Metadata.Uid != actor.Metadata.Uid {
		t.Fatal("test must retain actor incarnation")
	}
}

func TestSupervisorRestoreOnlyCredentialFreeGolden(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	template := supervisorbootstraptest.Template()
	template.Status = &ateapipb.ActorTemplateStatus{GoldenSnapshotStatus: &ateapipb.GoldenSnapshotStatus{GoldenTag: &ateapipb.ObjectRef{Atespace: "team-a", Name: "golden"}}}
	uri := someActorSnapshotURI(t, "gs://bucket/golden-root", resources.GoldenActorAtespace, "golden-1")
	_, err := persistence.CreateTag(ctx, &ateapipb.Tag{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "golden"}, Status: &ateapipb.TagStatus{ActorTemplateUid: template.Metadata.Uid, Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: uri, ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL}}})
	if err != nil {
		t.Fatal(err)
	}
	w := &ActorWorkflow{store: persistence}
	actor := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a"}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ExternalSnapshot: &ateapipb.ExternalSnapshot{SnapshotUri: uri, ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL}}}
	if err := w.validateSupervisorRestore(ctx, actor, template); err != nil {
		t.Fatalf("initial golden: %v", err)
	}
	actor.Status.SupervisorActivation = &ateapipb.SupervisorActivation{ActivationId: "retained"}
	if err := w.validateSupervisorRestore(ctx, actor, template); err == nil {
		t.Fatal("prior actor memory accepted")
	}
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RESUMING
	if err := w.validateSupervisorRestore(ctx, actor, template); err != nil {
		t.Fatalf("same activation retry: %v", err)
	}
	actor.Status.ExternalSnapshot.SnapshotUri = someActorSnapshotURI(t, "gs://bucket/actor-root", "team-a", "other")
	if err := w.validateSupervisorRestore(ctx, actor, template); err == nil {
		t.Fatal("arbitrary full snapshot accepted")
	}
	actor.Status.ExternalSnapshot = nil
	actor.Status.LocalSnapshot = &ateapipb.LocalSnapshot{ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL}
	if err := w.validateSupervisorRestore(ctx, actor, template); err == nil {
		t.Fatal("local memory snapshot accepted")
	}
	actor.Status.LocalSnapshot.ContentScope = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA
	if err := w.validateSupervisorRestore(ctx, actor, template); err != nil {
		t.Fatalf("data snapshot: %v", err)
	}
}

func TestSupervisorGoldenWorkloadHasNoActivation(t *testing.T) {
	template := supervisorbootstraptest.Template()
	actor := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: resources.GoldenActorAtespace}}
	spec, err := workloadSpecFromActorTemplate(template, actor)
	if err != nil {
		t.Fatal(err)
	}
	b := spec.Volumes[0].GetSystemInfo().DataSources[0].GetSupervisorBootstrap()
	if b.GetActivationId() != "" || b.GetWorkerUid() != "" {
		t.Fatal("golden carried activation")
	}
	actor.Metadata.Atespace = "team-a"
	if _, err := workloadSpecFromActorTemplate(template, actor); err == nil {
		t.Fatal("ordinary actor without activation admitted")
	}
}
