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

package controlapi

import (
	"context"
	"errors"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/apivalidation"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *RPCService) GetSupervisorTermination(ctx context.Context, req *ateapipb.GetSupervisorTerminationRequest) (*ateapipb.SupervisorTerminationReceipt, error) {
	if errs := apivalidation.ValidateGetActorRequest(ctx, &ateapipb.GetActorRequest{Actor: req.GetActor()}); len(errs) > 0 {
		return nil, resources.ToGRPCStatusError(errs)
	}
	if req.GetActor().GetAtespace() == "" || req.GetActor().GetName() == "" || req.GetActorUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "exact actor identity required")
	}
	if _, err := uuid.Parse(req.GetActivationId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, "activation identity required")
	}
	receipt, err := s.impl.GetSupervisorTermination(ctx, resources.ActorRefFromObjectRef(req.GetActor()), req.GetActorUid(), req.GetActivationId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "supervisor termination proof unavailable")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "supervisor termination proof unavailable")
	}
	return receipt, nil
}

func (s *ServiceImpl) GetSupervisorTermination(ctx context.Context, ref resources.ActorRef, actorUID, activationID string) (*ateapipb.SupervisorTerminationReceipt, error) {
	return s.store.GetSupervisorTermination(ctx, ref, actorUID, activationID)
}
