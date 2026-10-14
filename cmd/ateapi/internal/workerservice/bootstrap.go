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

package workerservice

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/apivalidation"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/ateletauth"
	"github.com/agent-substrate/substrate/internal/actoridjwt"
	"github.com/agent-substrate/substrate/internal/localjwtauthority"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/operation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// BootstrapConfig uses the existing actor issuer and current Kubernetes identity.
type BootstrapConfig struct {
	Issuer     string
	Pool       localjwtauthority.Pool
	Kubernetes kubernetes.Interface
}

// MintSupervisorBootstrap is deliberately separate from actor-wide credentials.
func (s *Server) MintSupervisorBootstrap(ctx context.Context, req *ateapipb.MintSupervisorBootstrapRequest) (*ateapipb.MintSupervisorBootstrapResponse, error) {
	caller, err := ateletauth.Authenticate(ctx, s.ateletSPIFFEID)
	if err != nil {
		return nil, err
	}
	deny := status.Error(codes.PermissionDenied, "supervisor bootstrap denied")
	if s.bootstrap == nil || s.bootstrap.Pool == nil || s.bootstrap.Kubernetes == nil || s.bootstrap.Issuer == "" {
		return nil, deny
	}
	if errs := apivalidation.Validate_MintSupervisorBootstrapRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil); len(errs) > 0 {
		return nil, deny
	}
	actor, err := s.store.GetActor(ctx, resources.ActorRefFromObjectRef(req.GetActor()))
	if err != nil {
		return nil, deny
	}
	if req.GetActor().GetAtespace() == resources.GoldenActorAtespace || actor.GetMetadata().GetUid() != req.GetActorUid() {
		return nil, deny
	}
	state := actor.GetStatus().GetState()
	if state != ateapipb.ActorState_ACTOR_STATE_RESUMING && state != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return nil, deny
	}
	activation := actor.GetStatus().GetSupervisorActivation()
	assignment := actor.GetStatus().GetWorkerAssignment()
	if activation == nil || activation.GetActivationId() != req.GetActivationId() || activation.GetWorkerUid() != req.GetWorkerUid() || assignment.GetWorkerPodUid() != req.GetWorkerUid() || assignment.GetNodeName() != caller.NodeName {
		return nil, deny
	}
	worker, err := s.store.GetWorker(ctx, assignment.GetWorker().GetName())
	if err != nil || worker.GetWorkerPodUid() != req.GetWorkerUid() || worker.GetNodeName() != caller.NodeName || worker.GetStatus().GetState() != ateapipb.WorkerState_WORKER_STATE_ACTIVE {
		return nil, deny
	}
	bound, err := s.store.GetWorkerAssignment(ctx, worker.GetMetadata().GetName(), req.GetActorUid())
	if err != nil || !proto.Equal(bound.GetActor(), req.GetActor()) || bound.GetActorUid() != req.GetActorUid() {
		return nil, deny
	}
	node, err := s.bootstrap.Kubernetes.CoreV1().Nodes().Get(ctx, caller.NodeName, metav1.GetOptions{})
	if err != nil || string(node.UID) != caller.NodeUID || node.DeletionTimestamp != nil {
		return nil, deny
	}
	pod, err := s.bootstrap.Kubernetes.CoreV1().Pods(worker.GetWorkerNamespace()).Get(ctx, worker.GetWorkerPod(), metav1.GetOptions{})
	if err != nil || string(pod.UID) != req.GetWorkerUid() || pod.Spec.NodeName != caller.NodeName || pod.DeletionTimestamp != nil {
		return nil, deny
	}
	template, err := s.store.GetActorTemplate(ctx, resources.ActorTemplateRefFromObjectRef(actor.GetActorTemplate()))
	if err != nil {
		return nil, deny
	}
	expected, err := supervisorbootstrap.Describe(template)
	if err != nil || expected == nil {
		return nil, deny
	}
	expected.ActivationId, expected.WorkerUid = activation.GetActivationId(), activation.GetWorkerUid()
	if !proto.Equal(expected, activation) {
		return nil, deny
	}
	// Re-read after external identity observations; an intervening actor transition
	// cannot receive a token. Consumers also require this exact current generation.
	current, err := s.store.GetActor(ctx, resources.ActorRefFromObjectRef(req.GetActor()))
	if err != nil || current.GetMetadata().GetVersion() != actor.GetMetadata().GetVersion() || current.GetMetadata().GetUid() != req.GetActorUid() {
		return nil, deny
	}
	now := time.Now().Truncate(time.Second)
	claims := &actoridjwt.Claims{Issuer: s.bootstrap.Issuer, Subject: "actor/" + req.GetActor().GetAtespace() + "/" + req.GetActor().GetName(), Audiences: []string{supervisorbootstrap.Audience}, IssuedAt: now, NotBefore: now, Expiration: now.Add(300 * time.Second), JTI: rand.Text(), Substrate: actoridjwt.SubstrateClaims{Atespace: req.GetActor().GetAtespace(), ActorName: req.GetActor().GetName(), ActorUID: req.GetActorUid(), Supervisor: &actoridjwt.SupervisorClaims{SchemaVersion: supervisorbootstrap.Schema, Purpose: supervisorbootstrap.Purpose, Profile: activation.GetProfile(), ActivationID: activation.GetActivationId(), WorkerUID: activation.GetWorkerUid(), NodeName: caller.NodeName, NodeUID: caller.NodeUID, TemplateUID: activation.GetTemplateUid(), TemplateDigest: activation.GetTemplateDigest(), ImageDigest: activation.GetImageDigest(), LaunchDigest: activation.GetLaunchDigest()}}}
	token, err := s.bootstrap.Pool.SignJWT(claims)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "supervisor signer unavailable")
	}
	return &ateapipb.MintSupervisorBootstrapResponse{Token: token}, nil
}
