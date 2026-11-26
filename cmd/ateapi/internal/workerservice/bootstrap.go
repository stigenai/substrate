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

package workerservice

import (
	"context"
	"crypto/rand"
	"log/slog"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/apivalidation"
	"go.ate.dev/substrate/cmd/ateapi/internal/ateletauth"
	"go.ate.dev/substrate/internal/actoridjwt"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/supervisorbootstrap"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// supervisorBootstrapLifetime is how long a bootstrap token is valid. The
// supervisor exchanges it once, right after it starts.
const supervisorBootstrapLifetime = 300 * time.Second

// MintSupervisorBootstrap is deliberately separate from the actor-wide JWT
// RPCs: it issues only to the atelet that holds the current activation.
func (s *Server) MintSupervisorBootstrap(ctx context.Context, req *ateapi.MintSupervisorBootstrapRequest) (*ateapi.MintSupervisorBootstrapResponse, error) {
	caller, err := ateletauth.Authenticate(ctx, s.ateletSPIFFEID)
	if err != nil {
		return nil, err
	}
	// Every refusal looks the same to the caller; the reason goes to the log.
	deny := func(reason string) error {
		slog.WarnContext(ctx, "Denied a supervisor bootstrap request", slog.String("reason", reason), slog.String("caller_node", caller.NodeName))
		return apierror.PermissionDenied("supervisor bootstrap denied")
	}
	if s.kube == nil || s.actorIDJWTPool == nil || s.actorJWTIssuer == "" {
		return nil, deny("issuer not configured")
	}
	if errs := apivalidation.ValidateMintSupervisorBootstrapRequest(ctx, req); len(errs) > 0 {
		return nil, deny("invalid request")
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	actor, err := s.store.GetActor(ctx, actorRef)
	if err != nil {
		return nil, deny("actor not readable")
	}
	if actor.GetMetadata().GetUid() != req.GetActorUid() {
		return nil, deny("actor UID differs")
	}
	if state := actor.GetStatus().GetState(); state != ateapi.ActorState_ACTOR_STATE_RESUMING && state != ateapi.ActorState_ACTOR_STATE_RUNNING {
		return nil, deny("actor is not starting or running")
	}
	activation := actor.GetStatus().GetSupervisorActivation()
	assignment := actor.GetStatus().GetWorkerAssignment()
	if activation == nil || activation.GetActivationId() != req.GetActivationId() || activation.GetWorkerUid() != req.GetWorkerUid() ||
		assignment.GetWorkerPodUid() != req.GetWorkerUid() || assignment.GetNodeName() != caller.NodeName {
		return nil, deny("activation or assignment differs")
	}
	worker, err := s.store.GetWorker(ctx, assignment.GetWorker().GetName())
	if err != nil || worker.GetWorkerPodUid() != req.GetWorkerUid() || worker.GetNodeName() != caller.NodeName || worker.GetStatus().GetState() != ateapi.WorkerState_WORKER_STATE_ACTIVE {
		return nil, deny("worker differs or is not active")
	}
	bound, err := s.store.GetWorkerAssignment(ctx, worker.GetMetadata().GetName(), req.GetActorUid())
	if err != nil || !proto.Equal(bound.GetActor(), req.GetActor()) || bound.GetActorUid() != req.GetActorUid() {
		return nil, deny("worker does not hold the actor")
	}
	node, err := s.kube.CoreV1().Nodes().Get(ctx, caller.NodeName, metav1.GetOptions{})
	if err != nil || string(node.UID) != caller.NodeUID || node.DeletionTimestamp != nil {
		return nil, deny("node identity differs")
	}
	pod, err := s.kube.CoreV1().Pods(worker.GetWorkerNamespace()).Get(ctx, worker.GetWorkerPod(), metav1.GetOptions{})
	if err != nil || string(pod.UID) != req.GetWorkerUid() || pod.Spec.NodeName != caller.NodeName || pod.DeletionTimestamp != nil {
		return nil, deny("worker pod identity differs")
	}
	template, err := s.store.GetActorTemplate(ctx, resources.ActorTemplateRefFromObjectRef(actor.GetActorTemplate()))
	if err != nil || supervisorbootstrap.IsGoldenActor(template, actor) {
		return nil, deny("template not readable or actor is golden")
	}
	expected, err := supervisorbootstrap.Describe(template)
	if err != nil || expected == nil {
		return nil, deny("template is not a protected supervisor")
	}
	expected.ActivationId, expected.WorkerUid = activation.GetActivationId(), activation.GetWorkerUid()
	if !proto.Equal(expected, activation) {
		return nil, deny("template differs from the recorded activation")
	}
	// Re-read after the external identity observations: an actor that changed
	// meanwhile gets no token.
	current, err := s.store.GetActor(ctx, actorRef)
	if err != nil || current.GetMetadata().GetVersion() != actor.GetMetadata().GetVersion() || current.GetMetadata().GetUid() != req.GetActorUid() {
		return nil, deny("actor changed during verification")
	}

	now := time.Now().Truncate(time.Second)
	meta := actor.GetMetadata()
	claims := &actoridjwt.Claims{
		Kind:       actoridjwt.KindActor,
		Issuer:     s.actorJWTIssuer,
		Subject:    actoridjwt.KindActor.SubjectPrefix() + "/" + meta.GetAtespace() + "/" + meta.GetName(),
		Audiences:  []string{supervisorbootstrap.Audience},
		Expiration: now.Add(supervisorBootstrapLifetime),
		NotBefore:  now,
		IssuedAt:   now,
		JTI:        rand.Text(),
		Substrate: actoridjwt.SubstrateClaims{
			Atespace:  meta.GetAtespace(),
			ActorName: meta.GetName(),
			ActorUID:  meta.GetUid(),
			Supervisor: &actoridjwt.SupervisorClaims{
				SchemaVersion:  supervisorbootstrap.Schema,
				Purpose:        supervisorbootstrap.Purpose,
				Profile:        activation.GetProfile(),
				ActivationID:   activation.GetActivationId(),
				WorkerUID:      activation.GetWorkerUid(),
				NodeName:       caller.NodeName,
				NodeUID:        caller.NodeUID,
				TemplateUID:    activation.GetTemplateUid(),
				TemplateDigest: activation.GetTemplateDigest(),
				ImageDigest:    activation.GetImageDigest(),
				LaunchDigest:   activation.GetLaunchDigest(),
			},
		},
	}
	token, err := s.actorIDJWTPool.SignJWT(claims)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to sign a supervisor bootstrap token", slog.Any("err", err))
		return nil, apierror.Unavailable("supervisor signer unavailable")
	}
	return &ateapi.MintSupervisorBootstrapResponse{Token: token}, nil
}
