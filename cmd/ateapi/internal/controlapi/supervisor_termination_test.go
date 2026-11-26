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
	"errors"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/cmd/ateapi/internal/store/storetest"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/supervisorbootstrap/supervisorbootstraptest"
	"go.ate.dev/substrate/internal/supervisortermination/supervisorterminationtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const stopWorkerPodUID = "worker-pod-uid"

// stoppingActor creates a deleting actor that holds a protected activation on
// node-1, and the acknowledgement a runtime would return for it.
func stoppingActor(t *testing.T, st store.Interface) (*ateapi.Actor, *ateapi.SupervisorTerminationAck) {
	t.Helper()
	r := supervisorterminationtest.Receipt()
	r.Activation.WorkerUid = stopWorkerPodUID
	r.Stop.Activation = proto.CloneOf(r.Activation)
	a, err := st.CreateActor(t.Context(), &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: "team-a", Name: "actor-a"},
		Status: &ateapi.ActorStatus{
			State:                ateapi.ActorState_ACTOR_STATE_DELETING,
			AssignedNode:         "node-1",
			SupervisorActivation: r.Activation,
			WorkerAssignment:     &ateapi.WorkerAssignment{NodeName: "node-1", WorkerPodUid: stopWorkerPodUID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Stop.ActorUid = a.GetMetadata().GetUid()
	return a, r.Stop
}

func TestSupervisorStopRetainedBeforeReleaseAndReplay(t *testing.T) {
	st, cleanup := storetest.SetupTestStore(t)
	defer cleanup()
	storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
	w, f := newWireCaptureWorkflow(t, st)
	a, stop := stoppingActor(t, st)
	f.stop = stop
	ref := resources.ActorRefFromActor(a)

	if _, err := w.retainAteletTermination(t.Context(), ref, a, nil); err != nil {
		t.Fatal(err)
	}
	retained, err := st.GetActor(t.Context(), ref)
	if err != nil || !proto.Equal(retained.GetStatus().GetSupervisorStop(), stop) || retained.GetStatus().GetWorkerAssignment() == nil {
		t.Fatal("proof not retained before assignment clear", err)
	}
	if retained.GetStatus().GetSupervisorStopNode() != "node-1" {
		t.Fatalf("stop node = %q", retained.GetStatus().GetSupervisorStopNode())
	}
	if !proto.Equal(f.terminate.GetSpec().GetSupervisorActivation(), a.GetStatus().GetSupervisorActivation()) {
		t.Fatal("activation not sent to atelet")
	}
	// A restarted workflow uses the durable acknowledgement, without another runtime call.
	w2 := &ActorWorkflow{store: st}
	if _, err = w2.retainAteletTermination(t.Context(), ref, retained, nil); err != nil {
		t.Fatal(err)
	}
	if f.terminateCalls != 1 {
		t.Fatalf("terminate calls = %d, want 1", f.terminateCalls)
	}
	retained.Status.SupervisorStop.Activation.ActivationId = "other"
	if _, err = w2.retainAteletTermination(t.Context(), ref, retained, nil); err == nil {
		t.Fatal("stale acknowledgement accepted")
	}
}

func TestSupervisorStopMissingAndMismatchNeverRetained(t *testing.T) {
	for _, kind := range []string{"unassigned", "notfound", "mismatch", "lostack"} {
		t.Run(kind, func(t *testing.T) {
			st, cleanup := storetest.SetupTestStore(t)
			defer cleanup()
			storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
			w, f := newWireCaptureWorkflow(t, st)
			a, stop := stoppingActor(t, st)
			ref := resources.ActorRefFromActor(a)
			switch kind {
			case "unassigned":
				a.Status.AssignedNode = ""
				a.Status.WorkerAssignment = nil
			case "notfound":
				f.terminateErr = status.Error(codes.NotFound, "absent")
			case "mismatch":
				stop.ActorUid = "another-actor"
				f.stop = stop
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
			if err != nil || persisted.GetStatus().GetSupervisorStop() != nil {
				t.Fatal("invalid proof persisted", err)
			}
		})
	}
}

func TestSupervisorWorkloadSpecCarriesClonedActivation(t *testing.T) {
	r := supervisorterminationtest.Receipt()
	actor := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: r.Atespace}, Status: &ateapi.ActorStatus{SupervisorActivation: r.Activation}}
	spec, err := workloadSpecFromActorTemplate(supervisorbootstraptest.Template(), actor, nil)
	if err != nil || !proto.Equal(spec.GetSupervisorActivation(), r.Activation) {
		t.Fatal("activation missing", err)
	}
	actor.Status.SupervisorActivation.ActivationId = "changed"
	if spec.GetSupervisorActivation().GetActivationId() == "changed" {
		t.Fatal("activation aliased the control record")
	}
	// The fallback spec, for an actor whose template is gone, carries it too.
	if !proto.Equal(fallbackWorkloadSpec(actor).GetSupervisorActivation(), actor.GetStatus().GetSupervisorActivation()) {
		t.Fatal("fallback spec lost the activation")
	}
}

func TestReadSupervisorTerminationExactLookup(t *testing.T) {
	st, cleanup := storetest.SetupTestStore(t)
	defer cleanup()
	storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
	a, stop := stoppingActor(t, st)
	ref := resources.ActorRefFromActor(a)
	workflow, host := newWireCaptureWorkflow(t, st)
	host.stop = stop
	if _, err := workflow.DeleteActor(t.Context(), ref, store.DeletePreconditions{UID: a.GetMetadata().GetUid()}); err != nil {
		t.Fatal(err)
	}
	if host.terminateCalls != 1 {
		t.Fatal("deletion did not call the bound host exactly once")
	}
	service := &RPCService{impl: newServiceImpl(st, nil)}
	req := &ateapi.ReadSupervisorTerminationRequest{
		Actor:        &ateapi.ObjectRef{Atespace: ref.Atespace, Name: ref.Name},
		ActorUid:     a.GetMetadata().GetUid(),
		ActivationId: a.GetStatus().GetSupervisorActivation().GetActivationId(),
	}
	got, err := service.ReadSupervisorTermination(t.Context(), req)
	if err != nil || got.GetReceipt().GetActorUid() != a.GetMetadata().GetUid() || !proto.Equal(got.GetReceipt().GetStop(), stop) {
		t.Fatal("RPC did not return the retained proof", err)
	}
	changed := proto.CloneOf(req)
	changed.ActorUid = "2c8d0a1e-3f4b-4c5d-8e6f-7a8b9c0d1e2f"
	if _, err = service.ReadSupervisorTermination(t.Context(), changed); apierror.Code(err) != codes.NotFound {
		t.Fatal("wrong uid accepted", err)
	}
	changed = proto.CloneOf(req)
	changed.ActivationId = ""
	if _, err = service.ReadSupervisorTermination(t.Context(), changed); apierror.Code(err) != codes.InvalidArgument {
		t.Fatal("missing activation accepted", err)
	}
}

func TestProtectedDeleteRequiresExactUIDBeforeMutation(t *testing.T) {
	st, cleanup := storetest.SetupTestStore(t)
	defer cleanup()
	storetest.MustCreateAtespace(t, t.Context(), st, "team-a")
	a, _ := stoppingActor(t, st)
	ref := resources.ActorRefFromActor(a)
	// Back to a state a delete may start from.
	a, err := st.UpdateActor(t.Context(), ref, store.PreconditionFrom(a), func(toUpdate *ateapi.Actor) error {
		toUpdate.Status.State = ateapi.ActorState_ACTOR_STATE_RUNNING
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow := &ActorWorkflow{store: st}
	ctx := context.Background()
	if _, err := workflow.DeleteActor(ctx, ref, store.DeletePreconditions{}); apierror.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unbound protected deletion accepted: %v", err)
	}
	if _, err := workflow.DeleteActor(ctx, ref, store.DeletePreconditions{UID: "old-actor"}); apierror.Code(err) != codes.Aborted {
		t.Fatalf("stale protected deletion accepted: %v", err)
	}
	retained, err := st.GetActor(ctx, ref)
	if err != nil || !proto.Equal(retained, a) {
		t.Fatal("rejected deletion mutated the protected actor", err)
	}
}
