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
	"context"
	"fmt"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type bootstrapClient struct {
	ateapipb.WorkerServiceClient
	request *ateapipb.MintSupervisorBootstrapRequest
	err     error
}

func (c *bootstrapClient) MintSupervisorBootstrap(_ context.Context, req *ateapipb.MintSupervisorBootstrapRequest, _ ...grpc.CallOption) (*ateapipb.MintSupervisorBootstrapResponse, error) {
	c.request = req
	if c.err != nil {
		return nil, c.err
	}
	return &ateapipb.MintSupervisorBootstrapResponse{Token: "test-synthetic-token"}, nil
}

func protectedVolume(root string) *systemInfoVolume {
	return &systemInfoVolume{Name: "bootstrap", Root: root, Spec: &ateletpb.SystemInfoVolume{DataSources: []*ateletpb.SystemInfoDataSource{{DataSource: &ateletpb.SystemInfoDataSource_SupervisorBootstrap{SupervisorBootstrap: &ateletpb.SupervisorBootstrapDataSource{Profile: supervisorbootstrap.Profile, ActivationId: "activation", WorkerUid: "worker"}}}}}}
}
func TestSupervisorProjectionPrivate(t *testing.T) {
	r := newSystemInfoVolumeRefresher(nil, nil)
	client := &bootstrapClient{}
	r.bootstrap = client
	v := protectedVolume(filepath.Join(t.TempDir(), "protected"))
	ref := resources.ActorRef{Atespace: "team-a", Name: "actor"}
	if err := r.Register("actor-uid", ref, []*systemInfoVolume{v}); err != nil {
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
	if err := r.write(ref, "actor-uid", v); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(v.Root, supervisorbootstrap.FileName))
	if info.Mode().Perm() != 0o600 {
		t.Fatal("existing permissions not repaired")
	}
}
func TestSupervisorGoldenNeverMints(t *testing.T) {
	r := newSystemInfoVolumeRefresher(nil, nil)
	client := &bootstrapClient{}
	r.bootstrap = client
	v := protectedVolume(filepath.Join(t.TempDir(), "golden"))
	b := v.Spec.DataSources[0].GetSupervisorBootstrap()
	b.ActivationId = ""
	b.WorkerUid = ""
	if err := r.Register("golden-uid", resources.ActorRef{Atespace: resources.GoldenActorAtespace, Name: "golden"}, []*systemInfoVolume{v}); err != nil {
		t.Fatal(err)
	}
	if client.request != nil {
		t.Fatal("golden asked for credential")
	}
	entries, err := os.ReadDir(v.Root)
	if err != nil || len(entries) != 0 {
		t.Fatal("golden projection not empty")
	}
}
func TestSupervisorProjectionUnavailableFailsClosed(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			r := newSystemInfoVolumeRefresher(nil, nil)
			if configured {
				r.bootstrap = &bootstrapClient{err: fmt.Errorf("private-secret-should-not-log")}
			}
			v := protectedVolume(filepath.Join(t.TempDir(), "protected"))
			err := r.Register("actor-uid", resources.ActorRef{Atespace: "team-a", Name: "actor"}, []*systemInfoVolume{v})
			if err == nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatal("issuer failure not safely denied")
			}
			if _, err := os.Stat(filepath.Join(v.Root, supervisorbootstrap.FileName)); !os.IsNotExist(err) {
				t.Fatal("credential retained on failure")
			}
		})
	}
}
func TestSupervisorTargetAndCheckpointBoundary(t *testing.T) {
	v := protectedVolume("")
	spec := &ateletpb.WorkloadSpec{Volumes: []*ateletpb.Volume{{Name: "bootstrap", Source: &ateletpb.Volume_SystemInfo{SystemInfo: v.Spec}}}}
	if err := validateSupervisorTarget(spec, "worker", "team-a"); err != nil {
		t.Fatal(err)
	}
	if err := validateSupervisorTarget(spec, "other", "team-a"); err == nil {
		t.Fatal("wrong worker accepted")
	}
	if err := validateSupervisorTarget(spec, "worker", resources.GoldenActorAtespace); err == nil {
		t.Fatal("credential-bearing golden accepted")
	}
	req := &ateletpb.CheckpointRequest{Atespace: "team-a", ActorName: "actor", ActorUid: "actor-uid", TargetAteomUid: "33333333-3333-4333-8333-333333333333", Spec: spec, Scope: ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL}
	if err := validateCheckpointRequest(req); err == nil || !strings.Contains(err.Error(), "exclude guest memory") {
		t.Fatalf("Full snapshot boundary: %v", err)
	}
}
