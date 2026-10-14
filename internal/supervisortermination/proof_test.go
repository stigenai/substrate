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

package supervisortermination_test

import (
	"testing"

	"github.com/agent-substrate/substrate/internal/supervisortermination"
	"github.com/agent-substrate/substrate/internal/supervisortermination/supervisorterminationtest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func TestReceiptBinding(t *testing.T) {
	cases := map[string]func(*ateapipb.SupervisorTerminationReceipt){
		"schema": func(r *ateapipb.SupervisorTerminationReceipt) { r.SchemaVersion = "other" },
		"name":   func(r *ateapipb.SupervisorTerminationReceipt) { r.ActorName = "other" },
		"actor":  func(r *ateapipb.SupervisorTerminationReceipt) { r.ActorUid = "other" },
		"activation": func(r *ateapipb.SupervisorTerminationReceipt) {
			r.Activation.ActivationId = "55555555-5555-4555-8555-555555555555"
		},
		"worker":      func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop.Activation.WorkerUid = "other" },
		"template":    func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop.Activation.TemplateDigest = "other" },
		"image":       func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop.Activation.ImageDigest = "other" },
		"launch":      func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop.Activation.LaunchDigest = "other" },
		"missingstop": func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop = nil },
		"runtime":     func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop.Runtime = "absence" },
		"node":        func(r *ateapipb.SupervisorTerminationReceipt) { r.NodeName = "" },
		"time": func(r *ateapipb.SupervisorTerminationReceipt) {
			r.DeletedAt.Seconds = r.Stop.TerminatedAt.Seconds - 1
		},
		"niltime": func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop.TerminatedAt = nil },
		"ackid":   func(r *ateapipb.SupervisorTerminationReceipt) { r.Stop.AcknowledgementId = "" },
	}
	ref := &ateapipb.ObjectRef{Atespace: "team-a", Name: "actor-a"}
	original := supervisorterminationtest.Receipt()
	if err := supervisortermination.ValidateReceipt(ref, original.ActorUid, original.Activation.ActivationId, original); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := supervisorterminationtest.Receipt()
			mutate(r)
			if supervisortermination.ValidateReceipt(ref, original.ActorUid, original.Activation.ActivationId, r) == nil {
				t.Fatal("accepted mismatched receipt")
			}
		})
	}
}
