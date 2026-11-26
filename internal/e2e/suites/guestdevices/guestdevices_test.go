// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package guestdevices

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.ate.dev/substrate/api/ateapi/v1"
	atev1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"go.ate.dev/substrate/internal/e2e"
	"go.ate.dev/substrate/internal/resources"
)

// fuseResponse mirrors the probe's /fuse payload.
type fuseResponse struct {
	Exists   bool   `json:"exists"`
	Major    uint32 `json:"major"`
	Minor    uint32 `json:"minor"`
	Mounted  bool   `json:"mounted"`
	RootSize int64  `json:"rootSize"`
	Error    string `json:"error"`
}

// fuseRootSize is the root size the probe's own FUSE server reports.
const fuseRootSize = 4242

// TestGuestDevicesRefusedOnGvisor asks the API server to create a pool with
// guestDevices on a gvisor class. It runs in both lanes, because it needs no
// worker.
func TestGuestDevicesRefusedOnGvisor(t *testing.T) {
	ctx := context.Background()
	wp := &atev1.WorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: "guestdevices-gvisor", Namespace: "default"},
		Spec: atev1.WorkerPoolSpec{
			Replicas:    1,
			WorkerImage: "ateworker:latest",
			SandboxClasses: []atev1.WorkerPoolSandboxClass{{
				Name:         atev1.SandboxClassGvisor,
				GuestDevices: []atev1.GuestDevice{atev1.GuestDeviceFUSE},
			}},
		},
	}
	_, err := e2e.GetClients().SubstrateK8s.AteV1().WorkerPools("default").Create(ctx, wp, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err == nil || !strings.Contains(err.Error(), "guestDevices requires the microvm sandbox class") {
		t.Fatalf("Create(gvisor pool with guestDevices) error = %v, want the guestDevices refusal", err)
	}
}

// TestGuestDeviceFUSE resumes an actor with SYS_ADMIN on a microVM pool with
// guestDevices: [fuse], and one on a pool without it. The probe inside each
// actor reports /dev/fuse and mounts a FUSE filesystem that it serves itself.
func TestGuestDeviceFUSE(t *testing.T) {
	if !e2e.IsMicroVM() {
		t.Skip("guestDevices is microVM only; TestGuestDevicesRefusedOnGvisor covers gvisor")
	}
	if _, err := e2e.CheckEnv("KO_DOCKER_REPO"); err != nil {
		t.Fatalf("CheckEnv failed: %v", err)
	}
	ctx := context.Background()
	clients := e2e.GetClients()
	namespace, _ := e2e.DeploySubstrateFixture(t, ctx, clients, e2e.SubstrateFixtureManifests{
		Pool:     "internal/e2e/fixtures/guestdevices/guestdevices.yaml.tmpl",
		Template: "internal/e2e/fixtures/guestdevices/guestdevices-templates.yaml.tmpl",
	}, "guestdevices", nil)

	t.Run("pool with guestDevices fuse", func(t *testing.T) {
		got := probeFUSE(t, ctx, clients, namespace, "fuse")
		if !got.Exists || got.Major != 10 || got.Minor != 229 {
			t.Fatalf("/dev/fuse = %+v, want a char device 10:229", got)
		}
		if !got.Mounted || got.RootSize != fuseRootSize {
			t.Fatalf("FUSE mount = %+v, want mounted with root size %d", got, fuseRootSize)
		}
	})
	t.Run("pool without guestDevices", func(t *testing.T) {
		if got := probeFUSE(t, ctx, clients, namespace, "nofuse"); got.Exists {
			t.Fatalf("/dev/fuse = %+v, want none", got)
		}
	})
}

// probeFUSE creates and resumes an actor from template and returns its /fuse
// report.
func probeFUSE(t *testing.T, ctx context.Context, clients *e2e.Clients, namespace, template string) fuseResponse {
	t.Helper()
	ref := &ateapi.ObjectRef{Atespace: namespace, Name: template + "-actor"}
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: namespace, Name: ref.Name},
		ActorTemplate: &ateapi.ObjectRef{Atespace: namespace, Name: template},
	}}); err != nil {
		t.Fatalf("CreateActor %q: %v", ref.Name, err)
	}
	t.Cleanup(func() {
		// DeleteActor requires the actor to be suspended.
		_, _ = clients.SubstrateAPI.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref})
		_, _ = clients.SubstrateAPI.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref})
	})
	if _, err := e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapi.ResumeActorRequest{Actor: ref}); err != nil {
		t.Fatalf("ResumeActor %q: %v", ref.Name, err)
	}

	rc, err := e2e.NewRouterClient(t, ctx)
	if err != nil {
		t.Fatalf("NewRouterClient: %v", err)
	}
	defer rc.Close()
	resp, err := rc.Get(ctx, resources.ActorRef{Atespace: namespace, Name: ref.Name}, "/fuse")
	if err != nil {
		t.Fatalf("GET /fuse for %q: %v", ref.Name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /fuse for %q: status %d, body %q", ref.Name, resp.StatusCode, body)
	}
	t.Logf("GET /fuse for %q: %s", ref.Name, body)
	var out fuseResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding /fuse for %q: %v", ref.Name, err)
	}
	return out
}
