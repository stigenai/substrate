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
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/agent-substrate/substrate/internal/ateomcgroup"
	"github.com/agent-substrate/substrate/internal/ocispec"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// bindSupervisor retains the server-owned activation before any process starts.
// Callers hold the actor lifecycle lock through the corresponding Run/Restore.
func (s *AteomService) bindSupervisor(uid string, spec *ateompb.WorkloadSpec, dirs *ateompb.ActorDirs, runscPath string) {
	if spec.GetSupervisorActivation() == nil {
		return
	}
	s.actorsMu.Lock()
	defer s.actorsMu.Unlock()
	if hosted := s.actors[uid]; hosted != nil {
		hosted.supervisorSpec = proto.Clone(spec).(*ateompb.WorkloadSpec)
		hosted.supervisorDirs = proto.Clone(dirs).(*ateompb.ActorDirs)
		hosted.supervisorRunscPath = runscPath
	}
}

func (s *AteomService) supervisorStopBinding(req *ateompb.TerminateWorkloadRequest) (*ateapipb.SupervisorActivation, error) {
	s.actorsMu.RLock()
	defer s.actorsMu.RUnlock()
	hosted := s.actors[req.GetActorUid()]
	if req.GetSpec().GetSupervisorActivation() == nil && (hosted == nil || hosted.supervisorSpec == nil) {
		return nil, nil
	}
	if hosted == nil || hosted.supervisorSpec == nil || hosted.session == nil {
		return nil, status.Error(codes.FailedPrecondition, "protected actor has no retained running activation")
	}
	if hosted.supervisorRunscPath != req.GetRunscPath() || !proto.Equal(hosted.supervisorSpec, req.GetSpec()) || !proto.Equal(hosted.supervisorDirs, req.GetActorDirs()) || hosted.attribution.UID != req.GetActorUid() || hosted.attribution.Ref.Atespace != req.GetAtespace() || hosted.attribution.Ref.Name != req.GetActorName() {
		return nil, status.Error(codes.FailedPrecondition, "protected termination does not match hosted activation")
	}
	return proto.Clone(hosted.supervisorSpec.GetSupervisorActivation()).(*ateapipb.SupervisorActivation), nil
}

func (s *AteomService) stopSupervisorProcesses(ctx context.Context, req *ateompb.TerminateWorkloadRequest) (*ateapipb.SupervisorActivation, error) {
	activation, err := s.supervisorStopBinding(req)
	if err != nil || activation == nil {
		return activation, err
	}
	// A bare successful delete or missing workload is not process-stop proof.
	// The kernel boundary pins and reaps the exact host processes in the
	// actor's cgroup; the runtime's normal cleanup must also succeed afterward.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := ateomcgroup.KillAndReapLeafUnder(stopCtx, s.cgroupRoot, ocispec.GVisorCgroupLeaf(req.GetActorUid(), ocispec.PauseContainer)); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "protected actor termination is unproven")
	}
	return activation, nil
}

func supervisorStopped(uid string, activation *ateapipb.SupervisorActivation) *ateapipb.SupervisorTerminationAck {
	if activation == nil {
		return nil
	}
	return &ateapipb.SupervisorTerminationAck{
		SchemaVersion:     "substrate.supervisor-stop/v1",
		ActorUid:          uid,
		Activation:        activation,
		AcknowledgementId: uuid.NewString(),
		TerminatedAt:      timestamppb.Now(),
		Runtime:           "gvisor",
	}
}
