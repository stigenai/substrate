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

package main

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/utils/clock"

	"go.ate.dev/substrate/cmd/atelet/internal/snapshottracker"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/supervisortermination/supervisorterminationtest"
)

func TestSupervisorTerminationPropagation(t *testing.T) {
	const (
		actorUID     = "01234567-89ab-cdef-0123-456789abcdef"
		workerPodUID = "5f1c0d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f"
	)
	for _, kind := range []string{"exact", "mismatch", "notfound", "unreachable"} {
		t.Run(kind, func(t *testing.T) {
			useTempNodeDirs(t)
			r := supervisorterminationtest.Receipt()
			r.Stop.ActorUid = actorUID
			worker := &fakeWorker{supervisorStop: r.Stop, workerEpoch: 1}
			serveFakeWorker(t, worker)

			host := imageVolumeTestRegistry(t)
			image := host + "/actor:v1"
			pinnedImage := pushTestImage(t, image, singleFileLayer(t, "bin/app", "app"))
			release := newGVisorTestRelease(t)
			s := &Atelet{
				workerDialer:      newWorkerDialer(1),
				imageCache:        newImageVolumeStore(t),
				sandboxAssetCache: newTestAssetCache(t),
				anonGCSClient:     fakeObjectStorage{data: release.tarball},
				systemInfoVolumes: newSystemInfoVolumeRefresher(nil, nil),
				snapshots:         snapshottracker.New(t.Context(), nil, clock.RealClock{}),
				checkpointTimeout: defaultCheckpointTimeout,
			}
			spec := &ateletpb.WorkloadSpec{
				SupervisorActivation: r.Activation,
				Containers:           []*ateletpb.Container{{Name: "app", Image: pinnedImage, Command: []string{"/bin/app"}}},
			}
			sandboxRec := testSandboxRecord(release)
			advertiseRuntime(t, s, sandboxRec)
			if _, err := s.Run(t.Context(), &ateletpb.RunRequest{
				Atespace: r.Atespace, ActorName: r.ActorName, ActorUid: actorUID,
				ActorTemplateAtespace: "default", ActorTemplateName: "counter",
				WorkerPodUid: workerPodUID, SandboxRuntime: runtimeFor(t, sandboxRec), Spec: spec,
			}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "mismatch":
				worker.supervisorStop.ActorUid = "other"
			case "notfound":
				worker.supervisorStop = nil
				worker.onTerminate = func() error { return status.Error(codes.NotFound, "absent") }
			case "unreachable":
				worker.supervisorStop = nil
				worker.onTerminate = func() error { return status.Error(codes.Unavailable, "lost response") }
			}
			response, err := s.Terminate(t.Context(), &ateletpb.TerminateRequest{
				Atespace: r.Atespace, ActorName: r.ActorName, ActorUid: actorUID,
				ActorTemplateAtespace: "default", ActorTemplateName: "counter",
				WorkerPodUid: workerPodUID, Spec: spec,
			})
			switch kind {
			case "mismatch":
				if err == nil || response.GetSupervisorStop() != nil {
					t.Fatal("a mismatched stop was acknowledged", err)
				}
			case "notfound", "unreachable":
				// Atelet cannot tell the worker is gone from the worker having
				// stopped the activation, so neither becomes proof.
				if err != nil || response.GetSupervisorStop() != nil {
					t.Fatalf("absence became proof or failed the terminate: %v %v", response, err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if !proto.Equal(response.GetSupervisorStop(), r.Stop) || !proto.Equal(worker.terminatedSpec.GetSupervisorActivation(), r.Activation) {
					t.Fatal("termination identity not propagated")
				}
			}
		})
	}
}

func TestSupervisorActivationProjectionIsCloned(t *testing.T) {
	r := supervisorterminationtest.Receipt()
	spec := &ateletpb.WorkloadSpec{SupervisorActivation: r.Activation}
	out, err := buildWorkerWorkloadSpec(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec.SupervisorActivation.ActivationId = "changed"
	if out.GetSupervisorActivation().GetActivationId() == "changed" {
		t.Fatal("runtime identity aliased the request")
	}
}
