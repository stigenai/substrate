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

// Package supervisorterminationtest supplies synthetic public identity only.
// These values exercise transport/persistence; they do not prove a host stop.
package supervisorterminationtest

import (
	"time"

	"github.com/agent-substrate/substrate/internal/supervisorbootstrap"
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap/supervisorbootstraptest"
	"github.com/agent-substrate/substrate/internal/supervisortermination"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func Receipt() *ateapipb.SupervisorTerminationReceipt {
	a, err := supervisorbootstrap.Describe(supervisorbootstraptest.Template())
	if err != nil {
		panic(err)
	}
	a.ActivationId = "22222222-2222-4222-8222-222222222222"
	a.WorkerUid = "worker-uid"
	stop := &ateapipb.SupervisorTerminationAck{SchemaVersion: supervisortermination.AckSchema, ActorUid: "actor-uid", Activation: proto.CloneOf(a), AcknowledgementId: "33333333-3333-4333-8333-333333333333", TerminatedAt: timestamppb.New(time.Unix(1700000000, 0)), Runtime: "gvisor"}
	return &ateapipb.SupervisorTerminationReceipt{SchemaVersion: supervisortermination.ReceiptSchema, Atespace: "team-a", ActorName: "actor-a", ActorUid: stop.ActorUid, Activation: a, NodeName: "node-a", Stop: stop, DeletionId: "44444444-4444-4444-8444-444444444444", DeletedAt: timestamppb.New(time.Unix(1700000001, 0))}
}
