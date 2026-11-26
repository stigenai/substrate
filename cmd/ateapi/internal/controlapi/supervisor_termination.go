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

package controlapi

import (
	"context"
	"errors"
	"fmt"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/apivalidation"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
)

// ReadSupervisorTermination returns the receipt committed with the deletion of
// one exact actor incarnation and activation.
func (s *RPCService) ReadSupervisorTermination(ctx context.Context, req *ateapi.ReadSupervisorTerminationRequest) (*ateapi.ReadSupervisorTerminationResponse, error) {
	if errs := apivalidation.ValidateReadSupervisorTerminationRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	receipt, err := s.impl.GetSupervisorTermination(ctx, resources.ActorRefFromObjectRef(req.GetActor()), req.GetActorUid(), req.GetActivationId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("supervisor termination proof unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("while reading supervisor termination proof: %w", err)
	}
	return &ateapi.ReadSupervisorTerminationResponse{Receipt: receipt}, nil
}

func (s *ServiceImpl) GetSupervisorTermination(ctx context.Context, ref resources.ActorRef, actorUID, activationID string) (*ateapi.SupervisorTerminationReceipt, error) {
	return s.store.GetSupervisorTermination(ctx, ref, actorUID, activationID)
}
