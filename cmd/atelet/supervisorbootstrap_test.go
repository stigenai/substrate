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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/supervisorbootstrap"
	"google.golang.org/grpc"
)

type bootstrapClient struct {
	ateapi.WorkerServiceClient
	request *ateapi.MintSupervisorBootstrapRequest
	err     error
}

func (c *bootstrapClient) MintSupervisorBootstrap(_ context.Context, req *ateapi.MintSupervisorBootstrapRequest, _ ...grpc.CallOption) (*ateapi.MintSupervisorBootstrapResponse, error) {
	c.request = req
	if c.err != nil {
		return nil, c.err
	}
	return &ateapi.MintSupervisorBootstrapResponse{Token: "test-synthetic-token"}, nil
}

func protectedVolume(root string) *systemInfoVolume {
	return &systemInfoVolume{Name: "bootstrap", Root: root, Spec: &ateletpb.SystemInfoVolumeSource{DataSources: []*ateletpb.SystemInfoDataSource{{SupervisorBootstrap: &ateletpb.SupervisorBootstrapDataSource{Profile: supervisorbootstrap.Profile, ActivationId: "activation", WorkerUid: "worker"}}}}}
}

func TestSupervisorProjectionPrivate(t *testing.T) {
	client := &bootstrapClient{}
	r := newSystemInfoVolumeRefresher(nil, nil, withSupervisorBootstrap(client))
	v := protectedVolume(filepath.Join(t.TempDir(), "protected"))
	ref := resources.ActorRef{Atespace: "team-a", Name: "actor"}
	if _, err := r.Register(t.Context(), "actor-uid", ref, []*systemInfoVolume{v}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{v.Root: 0o700, filepath.Join(v.Root, supervisorbootstrap.FileName): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("private mode: %v", err)
		}
	}
	if client.request.GetActorUid() != "actor-uid" || client.request.GetActivationId() != "activation" || client.request.GetWorkerUid() != "worker" {
		t.Fatal("issuance binding missing")
	}
	content, err := os.ReadFile(filepath.Join(v.Root, supervisorbootstrap.FileName))
	if err != nil || string(content) != "test-synthetic-token" {
		t.Fatal("projection missing")
	}
	// Correct a permissive existing file without exposing a new token publicly.
	if err := os.Chmod(filepath.Join(v.Root, supervisorbootstrap.FileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.write(t.Context(), ref, "actor-uid", v); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(v.Root, supervisorbootstrap.FileName))
	if info.Mode().Perm() != 0o600 {
		t.Fatal("existing permissions not repaired")
	}
}

func TestSupervisorEmptyActivationNeverMints(t *testing.T) {
	client := &bootstrapClient{}
	r := newSystemInfoVolumeRefresher(nil, nil, withSupervisorBootstrap(client))
	v := protectedVolume(filepath.Join(t.TempDir(), "golden"))
	b := v.Spec.DataSources[0].GetSupervisorBootstrap()
	b.ActivationId = ""
	b.WorkerUid = ""
	if _, err := r.Register(t.Context(), "golden-uid", resources.ActorRef{Atespace: "team-a", Name: "golden"}, []*systemInfoVolume{v}); err != nil {
		t.Fatal(err)
	}
	if client.request != nil {
		t.Fatal("golden asked for a token")
	}
	entries, err := os.ReadDir(v.Root)
	if err != nil || len(entries) != 0 {
		t.Fatal("golden projection not empty")
	}
}

func TestSupervisorProjectionUnavailableFailsClosed(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			var opts []refresherOption
			if configured {
				opts = append(opts, withSupervisorBootstrap(&bootstrapClient{err: fmt.Errorf("private-secret-should-not-log")}))
			}
			r := newSystemInfoVolumeRefresher(nil, nil, opts...)
			v := protectedVolume(filepath.Join(t.TempDir(), "protected"))
			_, err := r.Register(t.Context(), "actor-uid", resources.ActorRef{Atespace: "team-a", Name: "actor"}, []*systemInfoVolume{v})
			if err == nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatal("issuer failure not safely denied")
			}
			if _, err := os.Stat(filepath.Join(v.Root, supervisorbootstrap.FileName)); !os.IsNotExist(err) {
				t.Fatal("token retained on failure")
			}
		})
	}
}

func TestSupervisorTargetAndSnapshotBoundary(t *testing.T) {
	spec := &ateletpb.WorkloadSpec{Volumes: []*ateletpb.Volume{{Name: "bootstrap", SystemInfo: protectedVolume("").Spec}}}
	if err := validateSupervisorTarget(spec, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := validateSupervisorTarget(spec, "other"); err == nil {
		t.Fatal("wrong worker accepted")
	}
	golden := protectedVolume("").Spec
	golden.DataSources[0].GetSupervisorBootstrap().ActivationId = ""
	golden.DataSources[0].GetSupervisorBootstrap().WorkerUid = ""
	if err := validateSupervisorTarget(&ateletpb.WorkloadSpec{Volumes: []*ateletpb.Volume{{Name: "bootstrap", SystemInfo: golden}}}, "worker"); err != nil {
		t.Fatalf("empty projection: %v", err)
	}
	golden.DataSources[0].GetSupervisorBootstrap().ActivationId = "activation"
	if err := validateSupervisorTarget(&ateletpb.WorkloadSpec{Volumes: []*ateletpb.Volume{{Name: "bootstrap", SystemInfo: golden}}}, "worker"); err == nil {
		t.Fatal("activation without a worker accepted")
	}

	memory := &ateletpb.Snapshot{Fidelity: ateletpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY}
	rootfs := &ateletpb.Snapshot{Fidelity: ateletpb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS}
	if err := validateSupervisorSnapshot(spec, memory); err == nil || !strings.Contains(err.Error(), "exclude guest memory") {
		t.Fatalf("memory snapshot boundary: %v", err)
	}
	if err := validateSupervisorSnapshot(spec, rootfs); err != nil {
		t.Fatalf("rootfs snapshot: %v", err)
	}
	if err := validateSupervisorSnapshot(&ateletpb.WorkloadSpec{}, memory); err != nil {
		t.Fatalf("ordinary spec: %v", err)
	}
}
