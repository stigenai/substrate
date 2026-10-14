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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/supervisortermination/supervisorterminationtest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type terminationReader struct {
	receipt *ateapipb.SupervisorTerminationReceipt
	err     error
	request *ateapipb.GetSupervisorTerminationRequest
}

func (f *terminationReader) GetSupervisorTermination(_ context.Context, r *ateapipb.GetSupervisorTerminationRequest, _ ...grpc.CallOption) (*ateapipb.SupervisorTerminationReceipt, error) {
	f.request = proto.CloneOf(r)
	return f.receipt, f.err
}
func TestSupervisorTerminationOutput(t *testing.T) {
	r := supervisorterminationtest.Receipt()
	req := &ateapipb.GetSupervisorTerminationRequest{Actor: &ateapipb.ObjectRef{Atespace: r.Atespace, Name: r.ActorName}, ActorUid: r.ActorUid, ActivationId: r.Activation.ActivationId}
	f := &terminationReader{receipt: r}
	var out bytes.Buffer
	if err := writeSupervisorTermination(t.Context(), f, req, &out); err != nil {
		t.Fatal(err)
	}
	decoded := &ateapipb.SupervisorTerminationReceipt{}
	if err := protojson.Unmarshal(out.Bytes(), decoded); err != nil || !proto.Equal(decoded, r) || !proto.Equal(f.request, req) {
		t.Fatal("changed proof or lookup", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["stop"].(map[string]any)["terminated_at"] != "2023-11-14T22:13:20Z" {
		t.Fatal("timestamp shape changed")
	}
	for _, kind := range []string{"missing", "error", "actor", "activation", "name", "runtime"} {
		t.Run(kind, func(t *testing.T) {
			f := &terminationReader{receipt: supervisorterminationtest.Receipt()}
			switch kind {
			case "missing":
				f.receipt = nil
			case "error":
				f.err = errors.New("private remote text")
			case "actor":
				f.receipt.ActorUid = "other"
			case "activation":
				f.receipt.Activation.ActivationId = "other"
			case "name":
				f.receipt.ActorName = "other"
			case "runtime":
				f.receipt.Stop.Runtime = "notfound"
			}
			var out bytes.Buffer
			err := writeSupervisorTermination(t.Context(), f, req, &out)
			if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "private remote") {
				t.Fatal("invalid proof emitted", err)
			}
		})
	}
}
