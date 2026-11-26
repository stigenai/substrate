//go:build linux

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
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/actorlock"
	"go.ate.dev/substrate/internal/ateworker/cgroup"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/roottest"
)

func supervisorFixture(t *testing.T) (*AteWorkerService, *ateworkerpb.TerminateWorkloadRequest) {
	t.Helper()
	req := validTerminateRequest()
	req.RunscPath = filepath.Join(req.ActorDirs.SandboxAssetsDir, "runsc")
	req.Spec.SupervisorActivation = &ateapi.SupervisorActivation{ActivationId: uuid.NewString(), WorkerUid: uuid.NewString(), TemplateUid: uuid.NewString(), TemplateDigest: "sha256:template", ImageDigest: "sha256:image", LaunchDigest: "sha256:launch", Profile: "fabriek-native-v1"}
	hosted := &hostedActor{attribution: testActor, session: &workloadSession{}}
	s := &AteWorkerService{locks: actorlock.New(), actors: map[string]*hostedActor{testActor.UID: hosted}, cgroupRoot: t.TempDir()}
	s.epoch.Store(1)
	s.bindSupervisor(testActor.UID, req.Spec, req.ActorDirs, req.RunscPath)
	return s, req
}

func TestSupervisorStopRequiresRetainedExactActivation(t *testing.T) {
	for _, change := range []string{"activation", "worker", "actor", "name", "spec", "dirs", "runsc", "omitted", "unhosted", "not-running"} {
		t.Run(change, func(t *testing.T) {
			s, req := supervisorFixture(t)
			switch change {
			case "activation":
				req.Spec.SupervisorActivation.ActivationId = uuid.NewString()
			case "worker":
				req.Spec.SupervisorActivation.WorkerUid = uuid.NewString()
			case "actor":
				req.ActorUid = uuid.NewString()
			case "name":
				req.ActorName = "other"
			case "spec":
				req.Spec.Containers[0].Name = "other"
			case "dirs":
				req.ActorDirs.OciBundleDir = filepath.Join(t.TempDir(), "other")
			case "runsc":
				req.RunscPath = filepath.Join(req.ActorDirs.SandboxAssetsDir, "other")
			case "omitted":
				req.Spec.SupervisorActivation = nil
			case "unhosted":
				delete(s.actors, req.ActorUid)
			case "not-running":
				s.actors[req.ActorUid].session = nil
			}
			if got, err := s.supervisorStopBinding(req); err == nil || got != nil {
				t.Fatalf("changed %s accepted: %v %v", change, got, err)
			}
		})
	}
	s, req := supervisorFixture(t)
	got, err := s.supervisorStopBinding(req)
	if err != nil || !proto.Equal(got, req.Spec.SupervisorActivation) {
		t.Fatalf("retained binding: %v", err)
	}
	got.Profile = "changed"
	if s.actors[req.ActorUid].supervisorSpec.SupervisorActivation.Profile != "fabriek-native-v1" {
		t.Fatal("binding returned mutable authority")
	}
	delete(s.actors, req.ActorUid)
	req.Spec.SupervisorActivation = nil
	if got, err := s.stopSupervisorProcesses(context.Background(), req); err != nil || got != nil {
		t.Fatalf("ordinary cleanup changed: %v", err)
	}
	if supervisorStopped(req.ActorUid, nil) != nil {
		t.Fatal("ordinary actor got protected ack")
	}
}

func TestSupervisorStopMissingKernelProofReturnsNoAcknowledgement(t *testing.T) {
	s, req := supervisorFixture(t)
	activation, err := s.stopSupervisorProcesses(context.Background(), req)
	if err == nil || activation != nil {
		t.Fatalf("missing cgroup accepted: %v %v", activation, err)
	}
	if !s.actors[req.ActorUid].stopping.Load() {
		t.Fatal("deliberate stop not marked, so it would report as a crash")
	}
}

// Actual kernel and owned-process proof. The host processes stand in for the
// sandbox; no guest or runsc integration is claimed.
func TestSupervisorStopAcknowledgementAfterKernelStop(t *testing.T) {
	roottest.Require(t, "creates an owned cgroup and process fixture")
	s, req := supervisorFixture(t)
	s.cgroupRoot = cgroup.Root
	path := s.sandboxLeaf(req.ActorUid)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	var cmds []*exec.Cmd
	for range 2 {
		cmd := exec.Command("sleep", "300")
		cmd.SysProcAttr = &unix.SysProcAttr{UseCgroupFD: true, CgroupFD: int(dir.Fd())}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	}
	start := time.Now()
	activation, err := s.stopSupervisorProcesses(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range cmds {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", cmd.Process.Pid)); !os.IsNotExist(err) {
			t.Fatalf("owned process survived or was not reaped: %v", err)
		}
	}
	ack := supervisorStopped(req.ActorUid, activation)
	if ack.SchemaVersion != "substrate.supervisor-stop/v1" || ack.ActorUid != req.ActorUid || ack.Runtime != "gvisor" || !proto.Equal(ack.Activation, req.Spec.SupervisorActivation) || ack.TerminatedAt.AsTime().Before(start) {
		t.Fatalf("wrong acknowledgement: %v", ack)
	}
	if _, err := uuid.Parse(ack.AcknowledgementId); err != nil {
		t.Fatal(err)
	}
	if _, err := s.stopSupervisorProcesses(context.Background(), req); err == nil {
		t.Fatal("an emptied cgroup minted repeated proof")
	}
}
