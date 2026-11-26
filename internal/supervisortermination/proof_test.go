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

package supervisortermination_test

import (
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/supervisortermination"
	"go.ate.dev/substrate/internal/supervisortermination/supervisorterminationtest"
)

func TestReceiptBinding(t *testing.T) {
	cases := map[string]func(*ateapi.SupervisorTerminationReceipt){
		"schema": func(r *ateapi.SupervisorTerminationReceipt) { r.SchemaVersion = "other" },
		"name":   func(r *ateapi.SupervisorTerminationReceipt) { r.ActorName = "other" },
		"actor":  func(r *ateapi.SupervisorTerminationReceipt) { r.ActorUid = "other" },
		"activation": func(r *ateapi.SupervisorTerminationReceipt) {
			r.Activation.ActivationId = "55555555-5555-4555-8555-555555555555"
		},
		"worker":      func(r *ateapi.SupervisorTerminationReceipt) { r.Stop.Activation.WorkerUid = "other" },
		"template":    func(r *ateapi.SupervisorTerminationReceipt) { r.Stop.Activation.TemplateDigest = "other" },
		"image":       func(r *ateapi.SupervisorTerminationReceipt) { r.Stop.Activation.ImageDigest = "other" },
		"launch":      func(r *ateapi.SupervisorTerminationReceipt) { r.Stop.Activation.LaunchDigest = "other" },
		"missingstop": func(r *ateapi.SupervisorTerminationReceipt) { r.Stop = nil },
		"runtime":     func(r *ateapi.SupervisorTerminationReceipt) { r.Stop.Runtime = "absence" },
		"node":        func(r *ateapi.SupervisorTerminationReceipt) { r.NodeName = "" },
		"time": func(r *ateapi.SupervisorTerminationReceipt) {
			r.DeletedAt.Seconds = r.Stop.TerminatedAt.Seconds - 1
		},
		"niltime": func(r *ateapi.SupervisorTerminationReceipt) { r.Stop.TerminatedAt = nil },
		"ackid":   func(r *ateapi.SupervisorTerminationReceipt) { r.Stop.AcknowledgementId = "" },
	}
	ref := &ateapi.ObjectRef{Atespace: "team-a", Name: "actor-a"}
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
