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

package atepg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/supervisortermination/supervisorterminationtest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func stoppedActor(t *testing.T, p *Persistence) *ateapipb.Actor {
	t.Helper()
	r := supervisorterminationtest.Receipt()
	a, err := p.CreateActor(t.Context(), &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: r.Atespace, Name: r.ActorName}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING, SupervisorActivation: r.Activation, SupervisorStop: r.Stop, SupervisorStopNode: r.NodeName}})
	if err != nil {
		t.Fatal(err)
	}
	a, err = p.UpdateActor(t.Context(), resources.ActorRefFromActor(a), store.PreconditionFrom(a), func(a *ateapipb.Actor) error { a.Status.SupervisorStop.ActorUid = a.Metadata.Uid; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestSupervisorTerminationAtomicDurability(t *testing.T) {
	p := setupPostgresPersistence(t)
	createTestAtespace(t, p, "team-a")
	ctx := context.Background()
	a := stoppedActor(t, p)
	ref := resources.ActorRefFromActor(a)
	if _, err := p.GetSupervisorTermination(ctx, ref, a.Metadata.Uid, a.Status.SupervisorActivation.ActivationId); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("proof before deletion: %v", err)
	}
	if _, err := p.DeleteActor(ctx, ref, store.DeletePreconditions{UID: a.Metadata.Uid, Version: a.Metadata.Version}); err != nil {
		t.Fatal(err)
	}
	// A new persistence instance reads the committed proof after the actor is gone.
	restarted, err := NewPersistence(ctx, requirePool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	receipt, err := restarted.GetSupervisorTermination(ctx, ref, a.Metadata.Uid, a.Status.SupervisorActivation.ActivationId)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(receipt.Stop, a.Status.SupervisorStop) {
		t.Fatal("ACK changed")
	}
	again, err := restarted.GetSupervisorTermination(ctx, ref, a.Metadata.Uid, a.Status.SupervisorActivation.ActivationId)
	if err != nil || !proto.Equal(again, receipt) {
		t.Fatal("replay changed proof", err)
	}
	replacement, err := p.CreateActor(ctx, &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: ref.Atespace, Name: ref.Name}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetSupervisorTermination(ctx, ref, a.Metadata.Uid, a.Status.SupervisorActivation.ActivationId); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("old receipt allowed while replacement exists", err)
	}
	if _, err := p.DeleteActor(ctx, ref, store.DeletePreconditions{UID: a.Metadata.Uid}); !errors.Is(err, store.ErrUIDConflict) {
		t.Fatalf("replacement delete: %v", err)
	}
	if _, err := p.GetSupervisorTermination(ctx, ref, replacement.Metadata.Uid, a.Status.SupervisorActivation.ActivationId); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("replacement inherited proof", err)
	}
	if _, err := p.GetSupervisorTermination(ctx, ref, a.Metadata.Uid, "other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("other activation inherited proof", err)
	}
	for _, sql := range []string{`UPDATE supervisor_terminations SET proto=proto`, `DELETE FROM supervisor_terminations`} {
		if _, err := p.pool.Exec(ctx, sql); err == nil {
			t.Fatal("receipt mutation accepted")
		}
	}
}
func TestSupervisorTerminationInvalidProofRollsBackDelete(t *testing.T) {
	for _, kind := range []string{"actor", "activation", "worker", "future", "assigned", "state", "node"} {
		t.Run(kind, func(t *testing.T) {
			p := setupPostgresPersistence(t)
			createTestAtespace(t, p, "team-a")
			a := stoppedActor(t, p)
			ref := resources.ActorRefFromActor(a)
			a, err := p.UpdateActor(t.Context(), ref, store.PreconditionFrom(a), func(a *ateapipb.Actor) error {
				switch kind {
				case "actor":
					a.Status.SupervisorStop.ActorUid = "other"
				case "activation":
					a.Status.SupervisorActivation.ActivationId = "55555555-5555-4555-8555-555555555555"
				case "worker":
					a.Status.SupervisorStop.Activation.WorkerUid = "other"
				case "future":
					a.Status.SupervisorStop.TerminatedAt = timestamppb.New(time.Now().Add(time.Hour))
				case "assigned":
					a.Status.WorkerAssignment = &ateapipb.WorkerAssignment{}
				case "state":
					a.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
				case "node":
					a.Status.SupervisorStopNode = ""
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.DeleteActor(t.Context(), ref, store.DeletePreconditions{UID: a.Metadata.Uid, Version: a.Metadata.Version}); err == nil {
				t.Fatal("invalid deletion accepted")
			}
			if _, err := p.GetActor(t.Context(), ref); err != nil {
				t.Fatal("actor lost on rollback", err)
			}
			if _, err := p.GetSupervisorTermination(t.Context(), ref, a.Metadata.Uid, a.Status.SupervisorActivation.ActivationId); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("proof retained on rollback", err)
			}
		})
	}
}
func TestSupervisorTerminationOrdinaryDeleteHasNoProof(t *testing.T) {
	p := setupPostgresPersistence(t)
	createTestAtespace(t, p, "team-a")
	a := stoppedActor(t, p)
	ref := resources.ActorRefFromActor(a)
	a, err := p.UpdateActor(t.Context(), ref, store.PreconditionFrom(a), func(a *ateapipb.Actor) error { a.Status.SupervisorStop = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeleteActor(t.Context(), ref, store.DeletePreconditions{UID: a.Metadata.Uid}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetSupervisorTermination(t.Context(), ref, a.Metadata.Uid, a.Status.SupervisorActivation.ActivationId); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("absence became proof", err)
	}
}
