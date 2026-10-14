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
	"errors"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store/storetest"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap/supervisorbootstraptest"
	"github.com/agent-substrate/substrate/internal/supervisortermination/supervisorterminationtest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (f *capturingAtelet) Terminate(_ context.Context, req *ateletpb.TerminateRequest) (*ateletpb.TerminateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateCalls++
	f.terminate = proto.CloneOf(req)
	return &ateletpb.TerminateResponse{SupervisorStop: proto.CloneOf(f.stop)}, f.terminateErr
}
func TestSupervisorStopRetainedBeforeReleaseAndReplay(t *testing.T) {
	st, cleanup := storetest.SetupTestStore(t)
	defer cleanup()
	storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
	w, f := newWireCaptureWorkflow(t, st)
	r := supervisorterminationtest.Receipt()
	r.Activation.WorkerUid = "worker-pod-uid"
	r.Stop.Activation = proto.CloneOf(r.Activation)
	a, err := st.CreateActor(t.Context(), &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-a"}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING, SupervisorActivation: r.Activation, WorkerAssignment: &ateapipb.WorkerAssignment{NodeName: "node-1", WorkerPodUid: "worker-pod-uid"}}})
	if err != nil {
		t.Fatal(err)
	}
	r.Stop.ActorUid = a.Metadata.Uid
	f.stop = r.Stop
	ref := resources.ActorRefFromActor(a)
	_, err = w.retainAteletTermination(t.Context(), ref, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := st.GetActor(t.Context(), ref)
	if err != nil || !proto.Equal(retained.Status.SupervisorStop, r.Stop) || retained.Status.WorkerAssignment == nil {
		t.Fatal("proof not retained before assignment clear", err)
	}
	if !proto.Equal(f.terminate.Spec.SupervisorActivation, r.Activation) {
		t.Fatal("activation not sent to host")
	}
	// A restarted workflow uses durable ACK, without another runtime call.
	w2 := &ActorWorkflow{store: st}
	if _, err = w2.retainAteletTermination(t.Context(), ref, retained, nil); err != nil {
		t.Fatal(err)
	}
	if f.terminateCalls != 1 {
		t.Fatal("replayed runtime stop")
	}
	retained.Status.SupervisorStop.Activation.ActivationId = "other"
	if _, err = w2.retainAteletTermination(t.Context(), ref, retained, nil); err == nil {
		t.Fatal("stale ACK accepted")
	}
}
func TestSupervisorStopMissingAndMismatchNeverRetained(t *testing.T) {
	for _, kind := range []string{"unassigned", "notfound", "missing", "mismatch", "lostack"} {
		t.Run(kind, func(t *testing.T) {
			st, cleanup := storetest.SetupTestStore(t)
			defer cleanup()
			storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
			w, f := newWireCaptureWorkflow(t, st)
			r := supervisorterminationtest.Receipt()
			r.Activation.WorkerUid = "worker-pod-uid"
			r.Stop.Activation = proto.CloneOf(r.Activation)
			a, err := st.CreateActor(t.Context(), &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-a"}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING, SupervisorActivation: r.Activation, WorkerAssignment: &ateapipb.WorkerAssignment{NodeName: "node-1", WorkerPodUid: "worker-pod-uid"}}})
			if err != nil {
				t.Fatal(err)
			}
			ref := resources.ActorRefFromActor(a)
			switch kind {
			case "unassigned":
				a.Status.WorkerAssignment = nil
			case "notfound":
				f.terminateErr = status.Error(codes.NotFound, "absent")
			case "mismatch":
				f.stop = r.Stop
			case "lostack":
				f.terminateErr = errors.New("lost response")
			}
			result, err := w.retainAteletTermination(t.Context(), ref, a, nil)
			if (kind == "mismatch" || kind == "lostack") && err == nil {
				t.Fatal("ambiguous stop accepted")
			}
			if result.GetStatus().GetSupervisorStop() != nil {
				t.Fatal("absence became proof")
			}
			persisted, err := st.GetActor(t.Context(), ref)
			if err != nil || persisted.Status.SupervisorStop != nil {
				t.Fatal("invalid proof persisted", err)
			}
		})
	}
}

func TestSupervisorWorkloadSpecCarriesClonedActivation(t *testing.T) {
	r := supervisorterminationtest.Receipt()
	actor := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: r.Atespace}, Status: &ateapipb.ActorStatus{SupervisorActivation: r.Activation}}
	spec, err := workloadSpecFromActorTemplate(supervisorbootstraptest.Template(), actor)
	if err != nil || !proto.Equal(spec.GetSupervisorActivation(), r.Activation) {
		t.Fatal("activation missing", err)
	}
	actor.Status.SupervisorActivation.ActivationId = "changed"
	if spec.SupervisorActivation.ActivationId == "changed" {
		t.Fatal("activation aliased control record")
	}
}

func TestSupervisorTerminationRPCExactLookup(t *testing.T) {
	st, cleanup := storetest.SetupTestStore(t)
	defer cleanup()
	storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
	r := supervisorterminationtest.Receipt()
	r.Activation.WorkerUid = "worker-pod-uid"
	r.Stop.Activation = proto.CloneOf(r.Activation)
	a, err := st.CreateActor(t.Context(), &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: r.Atespace, Name: r.ActorName}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING, SupervisorActivation: r.Activation, WorkerAssignment: &ateapipb.WorkerAssignment{NodeName: "node-1", WorkerPodUid: "worker-pod-uid"}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := resources.ActorRefFromActor(a)
	workflow, host := newWireCaptureWorkflow(t, st)
	r.Stop.ActorUid = a.Metadata.Uid
	host.stop = r.Stop
	if _, err = workflow.DeleteActor(t.Context(), ref, true, store.DeletePreconditions{UID: a.Metadata.Uid}); err != nil {
		t.Fatal(err)
	}
	if host.terminateCalls != 1 {
		t.Fatal("deletion did not call the bound host exactly once")
	}
	service := &RPCService{impl: newServiceImpl(st, nil)}
	req := &ateapipb.GetSupervisorTerminationRequest{Actor: &ateapipb.ObjectRef{Atespace: ref.Atespace, Name: ref.Name}, ActorUid: a.Metadata.Uid, ActivationId: r.Activation.ActivationId}
	got, err := service.GetSupervisorTermination(t.Context(), req)
	if err != nil || got.ActorUid != a.Metadata.Uid {
		t.Fatal("RPC did not return retained proof", err)
	}
	changed := proto.CloneOf(req)
	changed.ActorUid = "other"
	if _, err = service.GetSupervisorTermination(t.Context(), changed); status.Code(err) != codes.NotFound {
		t.Fatal("wrong UID accepted", err)
	}
	changed = proto.CloneOf(req)
	changed.ActivationId = ""
	if _, err = service.GetSupervisorTermination(t.Context(), changed); status.Code(err) != codes.InvalidArgument {
		t.Fatal("missing activation accepted", err)
	}
}

func TestSupervisorDeleteRequiresExactUIDBeforeMutation(t *testing.T) {
	for _, activation := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-first-activation", true: "retained-activation"}[activation], func(t *testing.T) {
			st, cleanup := storetest.SetupTestStore(t)
			defer cleanup()
			storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
			template := supervisorbootstraptest.Template()
			template.Metadata = &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "protected"}
			if _, err := st.CreateActorTemplate(t.Context(), template); err != nil {
				t.Fatal(err)
			}
			seed := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "replacement"}, ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "protected"}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}}
			if activation {
				seed.Status.SupervisorActivation = supervisorterminationtest.Receipt().Activation
				// Retained activation still protects deletion when the template is gone.
				seed.ActorTemplate.Name = "missing"
			}
			actor, err := st.CreateActor(t.Context(), seed)
			if err != nil {
				t.Fatal(err)
			}
			workflow := &ActorWorkflow{store: st}
			ref := resources.ActorRefFromActor(actor)
			if _, err := workflow.DeleteActor(t.Context(), ref, true, store.DeletePreconditions{}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("unbound protected deletion accepted: %v", err)
			}
			if _, err := workflow.DeleteActor(t.Context(), ref, true, store.DeletePreconditions{UID: "old-actor"}); status.Code(err) != codes.Aborted {
				t.Fatalf("stale protected deletion accepted: %v", err)
			}
			retained, err := st.GetActor(t.Context(), ref)
			if err != nil || !proto.Equal(retained, actor) {
				t.Fatal("rejected deletion mutated the protected actor", err)
			}
		})
	}
}
