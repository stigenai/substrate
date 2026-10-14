//go:build linux

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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/actorlock"
	"github.com/agent-substrate/substrate/internal/actorlog"
	"github.com/agent-substrate/substrate/internal/ateomcgroup"
	"github.com/agent-substrate/substrate/internal/atunnel"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/roottest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

func supervisorFixture(t *testing.T) (*AteomService, *ateompb.TerminateWorkloadRequest) {
	t.Helper()
	uid := uuid.NewString()
	root := t.TempDir()
	dirs := &ateompb.ActorDirs{RootDir: root, OciBundleDir: filepath.Join(root, "bundles"), CheckpointDir: filepath.Join(root, "checkpoint"), RestoreDir: filepath.Join(root, "restore"), DurableDirVolumeMountsDir: filepath.Join(root, "durable"), SystemInfoVolumeRootsDir: filepath.Join(root, "system"), VolumesDir: filepath.Join(root, "volumes")}
	req := &ateompb.TerminateWorkloadRequest{ActorUid: uid, Atespace: "fixture", ActorName: "actor", ActorDirs: dirs, RunscPath: filepath.Join(root, "runsc-fixture"), Spec: &ateompb.WorkloadSpec{Containers: []*ateompb.Container{{Name: "app"}}, SupervisorActivation: &ateapipb.SupervisorActivation{ActivationId: uuid.NewString(), WorkerUid: uuid.NewString(), TemplateUid: uuid.NewString(), TemplateDigest: "sha256:template", ImageDigest: "sha256:image", LaunchDigest: "sha256:launch", Profile: "fabriek-native-v1"}}}
	hosted := &hostedActor{attribution: resources.ActorAttribution{UID: uid, Ref: resources.ActorRef{Atespace: req.Atespace, Name: req.ActorName}}}
	s := &AteomService{locks: actorlock.New(), actors: map[string]*hostedActor{uid: hosted}, atunnelIngress: &atunnel.Server{}, atunnelEgress: &atunnel.Egress{}, actorLogger: actorlog.NewActorLogger(io.Discard, false)}
	s.actorCgroups = true
	hosted.vm = &runningActor{chCmd: &exec.Cmd{Process: &os.Process{}}, vfsdCmd: &exec.Cmd{Process: &os.Process{}}}
	s.bindSupervisor(uid, req.Spec, dirs)
	return s, req
}

func TestSupervisorStopRequiresRetainedExactActivation(t *testing.T) {
	for _, change := range []string{"activation", "worker", "actor", "name", "spec", "dirs", "omitted", "unhosted", "not-running"} {
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
			case "omitted":
				req.Spec.SupervisorActivation = nil
			case "unhosted":
				delete(s.actors, req.ActorUid)
			case "not-running":
				s.actors[req.ActorUid].vm = nil
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
	response, err := s.TerminateWorkload(context.Background(), req)
	if err == nil || response != nil {
		t.Fatalf("missing cgroup accepted: %v %v", response, err)
	}
}

// Actual kernel/owned-process proof plus RPC cleanup wiring. The host processes
// stand in for runtime processes; no guest, runsc, or VMM integration is claimed.
func TestSupervisorStopAcknowledgementAfterKernelStop(t *testing.T) {
	roottest.Require(t, "creates an owned cgroup and process fixture")
	for _, failCleanup := range []bool{false} {
		t.Run(fmt.Sprint(failCleanup), func(t *testing.T) {
			s, req := supervisorFixture(t)
			name := req.ActorUid
			path := filepath.Join(ateomcgroup.Root, name)
			if err := os.Mkdir(path, 0700); err != nil {
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
			s.actors[req.ActorUid].vm.chCmd = cmds[0]
			s.actors[req.ActorUid].vm.vfsdCmd = cmds[1]
			script := "#!/bin/sh\nexit 0\n"
			if failCleanup {
				script = "#!/bin/sh\nexit 1\n"
			}
			if err := os.WriteFile(req.RunscPath, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			response, err := s.TerminateWorkload(context.Background(), req)
			for _, cmd := range cmds {
				if _, err := os.Stat(fmt.Sprintf("/proc/%d", cmd.Process.Pid)); !os.IsNotExist(err) {
					t.Fatalf("owned process survived/reap missing: %v", err)
				}
			}
			if failCleanup {
				if err == nil || response != nil {
					t.Fatal("failed cleanup returned acknowledgement")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ack := response.GetSupervisorStop()
			if ack == nil || ack.SchemaVersion != "substrate.supervisor-stop/v1" || ack.ActorUid != req.ActorUid || ack.Runtime != "microvm" || !proto.Equal(ack.Activation, req.Spec.SupervisorActivation) || ack.TerminatedAt.AsTime().Before(start) {
				t.Fatalf("wrong acknowledgement: %v", ack)
			}
			if _, err := uuid.Parse(ack.AcknowledgementId); err != nil {
				t.Fatal(err)
			}
			if again, err := s.TerminateWorkload(context.Background(), req); err == nil || again != nil {
				t.Fatal("missing hosted binding minted repeated proof")
			}
		})
	}
}
