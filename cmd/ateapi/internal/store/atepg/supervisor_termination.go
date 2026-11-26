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

package atepg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/supervisortermination"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The delete and immutable receipt are committed together; failed validation or
// insertion rolls back the actor deletion. Missing stop evidence creates none.
func retainSupervisorTermination(ctx context.Context, tx pgx.Tx, actor *ateapi.Actor) error {
	stop := actor.GetStatus().GetSupervisorStop()
	if stop == nil {
		return nil
	}
	if actor.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_DELETING || actor.GetStatus().GetWorkerAssignment() != nil {
		return fmt.Errorf("protected actor cleanup incomplete")
	}
	var deletedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&deletedAt); err != nil {
		return err
	}
	m := actor.GetMetadata()
	receipt := &ateapi.SupervisorTerminationReceipt{
		SchemaVersion: supervisortermination.ReceiptSchema, Atespace: m.GetAtespace(), ActorName: m.GetName(), ActorUid: m.GetUid(),
		Activation: proto.CloneOf(actor.GetStatus().GetSupervisorActivation()), NodeName: actor.GetStatus().GetSupervisorStopNode(), Stop: proto.CloneOf(stop),
		DeletionId: uuid.NewString(), DeletedAt: timestamppb.New(deletedAt),
	}
	if err := supervisortermination.ValidateReceipt(&ateapi.ObjectRef{Atespace: m.GetAtespace(), Name: m.GetName()}, m.GetUid(), receipt.GetActivation().GetActivationId(), receipt); err != nil {
		return err
	}
	raw, err := proto.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO supervisor_terminations (atespace, actor_name, actor_uid, activation_id, proto) VALUES ($1,$2,$3,$4,$5)`, m.GetAtespace(), m.GetName(), m.GetUid(), receipt.GetActivation().GetActivationId(), raw)
	return err
}

func (p *Persistence) GetSupervisorTermination(ctx context.Context, ref resources.ActorRef, actorUID, activationID string) (*ateapi.SupervisorTerminationReceipt, error) {
	var raw []byte
	err := p.pool.QueryRow(ctx, `SELECT proto FROM supervisor_terminations WHERE atespace=$1 AND actor_name=$2 AND actor_uid=$3 AND activation_id=$4 AND NOT EXISTS (SELECT 1 FROM actors WHERE actors.atespace=$1 AND actors.name=$2)`, ref.Atespace, ref.Name, actorUID, activationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	receipt := &ateapi.SupervisorTerminationReceipt{}
	// Receipts are immutable evidence, not defaulted mutable API resources.
	if err := proto.Unmarshal(raw, receipt); err != nil {
		return nil, err
	}
	if err := supervisortermination.ValidateReceipt(&ateapi.ObjectRef{Atespace: ref.Atespace, Name: ref.Name}, actorUID, activationID, receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}
