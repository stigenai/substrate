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

package main

import (
	"crypto/sha256"
	"fmt"
	"runtime"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/supervisortermination/supervisorterminationtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestSupervisorTerminationPropagation(t *testing.T) {
	for _, kind := range []string{"exact", "mismatch", "notfound", "lostack"} {
		t.Run(kind, func(t *testing.T) {
			useTempNodeDirs(t)
			r := supervisorterminationtest.Receipt()
			f := &fakeAteom{supervisorStop: r.Stop}
			serveFakeAteom(t, f)
			host := imageVolumeTestRegistry(t)
			image := host + "/actor:v1"
			pushTestImage(t, image, singleFileLayer(t, "bin/app", "app"))
			runsc := []byte("runsc binary")
			s := &AteomHerder{ateomDialer: newAteomDialer(1), imageCache: newImageVolumeStore(t), anonGCSClient: fakeObjectStorage{data: runsc}, systemInfoVolumes: newSystemInfoVolumeRefresher(nil, nil)}
			assets := &ateletpb.SandboxAssets{SandboxClass: "gvisor", PauseImage: image, Assets: map[string]*ateletpb.ArchAssets{runtime.GOARCH: {Files: map[string]*ateletpb.AssetFile{runscAssetName: {Url: "gs://test-bucket/runsc", Sha256: fmt.Sprintf("%x", sha256.Sum256(runsc))}}}}}
			spec := &ateletpb.WorkloadSpec{SupervisorActivation: r.Activation, Containers: []*ateletpb.Container{{Name: "app", Image: image, Command: []string{"/bin/app"}}}}
			if _, err := s.Run(t.Context(), &ateletpb.RunRequest{Atespace: r.Atespace, ActorName: r.ActorName, ActorUid: r.ActorUid, ActorTemplateAtespace: "team-a", ActorTemplateName: "template-1", TargetAteomUid: "worker-uid", SandboxAssets: assets, Spec: spec}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "mismatch":
				f.supervisorStop.ActorUid = "other"
			case "notfound":
				f.supervisorStop = nil
				f.terminateErr = status.Error(codes.NotFound, "absent")
			case "lostack":
				f.supervisorStop = nil
				f.terminateErr = status.Error(codes.Unavailable, "lost response")
			}
			response, err := s.Terminate(t.Context(), &ateletpb.TerminateRequest{Atespace: r.Atespace, ActorName: r.ActorName, ActorUid: r.ActorUid, ActorTemplateAtespace: "team-a", ActorTemplateName: "template-1", TargetAteomUid: "worker-uid", Spec: spec})
			if kind == "mismatch" || kind == "lostack" {
				if err == nil || response.GetSupervisorStop() != nil {
					t.Fatal("ambiguous stop acknowledged", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "notfound" {
				if response.GetSupervisorStop() != nil {
					t.Fatal("absence became proof")
				}
				return
			}
			if !proto.Equal(response.GetSupervisorStop(), r.Stop) || !proto.Equal(f.terminatedSpec.SupervisorActivation, r.Activation) {
				t.Fatal("termination identity not propagated")
			}
		})
	}
}
func TestSupervisorActivationProjectionIsCloned(t *testing.T) {
	r := supervisorterminationtest.Receipt()
	spec := &ateletpb.WorkloadSpec{SupervisorActivation: r.Activation}
	out, err := buildAteomWorkloadSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.SupervisorActivation.ActivationId = "changed"
	if out.SupervisorActivation.ActivationId == "changed" {
		t.Fatal("runtime identity aliased request")
	}
}
