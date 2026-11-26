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
	"errors"
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
	deny := func(reason error) error {
		slog.WarnContext(ctx, "Denied a supervisor bootstrap request", slog.Any("reason", reason), slog.String("caller_node", caller.NodeName))
		return apierror.PermissionDenied("supervisor bootstrap denied")
	}
	if s.kube == nil || s.actorIDJWTPool == nil || s.actorJWTIssuer == "" {
		return nil, deny(errors.New("issuer not configured"))
	}
	if errs := apivalidation.ValidateMintSupervisorBootstrapRequest(ctx, req); len(errs) > 0 {
		return nil, deny(errors.New("invalid request"))
	}
	actor, worker, err := s.bootstrapActor(ctx, req, caller)
	if err != nil {
		return nil, deny(err)
	}
	if err := s.bootstrapTemplateMatches(ctx, actor); err != nil {
		return nil, deny(err)
	}
	if err := s.currentBootstrapHost(ctx, caller, worker); err != nil {
		return nil, deny(err)
	}
	// Re-read after the external identity observations: an actor that changed
	// meanwhile gets no token.
	current, err := s.store.GetActor(ctx, resources.ActorRefFromObjectRef(req.GetActor()))
	if err != nil || current.GetMetadata().GetVersion() != actor.GetMetadata().GetVersion() || current.GetMetadata().GetUid() != req.GetActorUid() {
		return nil, deny(errors.New("actor changed during verification"))
	}

	claims := s.bootstrapClaims(actor, caller, time.Now().Truncate(time.Second))
	token, err := s.actorIDJWTPool.SignJWT(claims)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to sign a supervisor bootstrap token", slog.Any("err", err))
		return nil, apierror.Unavailable("supervisor signer unavailable")
	}
	return &ateapi.MintSupervisorBootstrapResponse{Token: token}, nil
}

// bootstrapActor returns the actor and worker the request names, when the
// actor is starting or running on the calling node with exactly the requested
// activation.
func (s *Server) bootstrapActor(ctx context.Context, req *ateapi.MintSupervisorBootstrapRequest, caller *ateletauth.Caller) (*ateapi.Actor, *ateapi.Worker, error) {
	actor, err := s.store.GetActor(ctx, resources.ActorRefFromObjectRef(req.GetActor()))
	if err != nil {
		return nil, nil, errors.New("actor not readable")
	}
	if actor.GetMetadata().GetUid() != req.GetActorUid() {
		return nil, nil, errors.New("actor UID differs")
	}
	if state := actor.GetStatus().GetState(); state != ateapi.ActorState_ACTOR_STATE_RESUMING && state != ateapi.ActorState_ACTOR_STATE_RUNNING {
		return nil, nil, errors.New("actor is not starting or running")
	}
	activation := actor.GetStatus().GetSupervisorActivation()
	assignment := actor.GetStatus().GetWorkerAssignment()
	if activation == nil || activation.GetActivationId() != req.GetActivationId() || activation.GetWorkerUid() != req.GetWorkerUid() ||
		assignment.GetWorkerPodUid() != req.GetWorkerUid() || assignment.GetNodeName() != caller.NodeName {
		return nil, nil, errors.New("activation or assignment differs")
	}
	worker, err := s.store.GetWorker(ctx, assignment.GetWorker().GetName())
	if err != nil || worker.GetWorkerPodUid() != req.GetWorkerUid() || worker.GetNodeName() != caller.NodeName || worker.GetStatus().GetState() != ateapi.WorkerState_WORKER_STATE_ACTIVE {
		return nil, nil, errors.New("worker differs or is not active")
	}
	bound, err := s.store.GetWorkerAssignment(ctx, worker.GetMetadata().GetName(), req.GetActorUid())
	if err != nil || !proto.Equal(bound.GetActor(), req.GetActor()) || bound.GetActorUid() != req.GetActorUid() {
		return nil, nil, errors.New("worker does not hold the actor")
	}
	return actor, worker, nil
}

// bootstrapTemplateMatches checks that the actor's current template still
// describes exactly the recorded activation, and that the actor is not the
// template's golden actor.
func (s *Server) bootstrapTemplateMatches(ctx context.Context, actor *ateapi.Actor) error {
	template, err := s.store.GetActorTemplate(ctx, resources.ActorTemplateRefFromObjectRef(actor.GetActorTemplate()))
	if err != nil {
		return errors.New("template not readable")
	}
	if supervisorbootstrap.IsGoldenActor(template, actor) {
		return errors.New("actor is golden")
	}
	activation := actor.GetStatus().GetSupervisorActivation()
	expected, err := supervisorbootstrap.Describe(template)
	if err != nil || expected == nil {
		return errors.New("template is not a protected supervisor")
	}
	expected.ActivationId, expected.WorkerUid = activation.GetActivationId(), activation.GetWorkerUid()
	if !proto.Equal(expected, activation) {
		return errors.New("template differs from the recorded activation")
	}
	return nil
}

// currentBootstrapHost checks the live Kubernetes objects: the caller's node,
// the worker Pod and the calling atelet's own Pod must all exist with the
// recorded UIDs, on the caller's node, and not be terminating.
func (s *Server) currentBootstrapHost(ctx context.Context, caller *ateletauth.Caller, worker *ateapi.Worker) error {
	node, err := s.kube.CoreV1().Nodes().Get(ctx, caller.NodeName, metav1.GetOptions{})
	if err != nil || string(node.UID) != caller.NodeUID || node.DeletionTimestamp != nil {
		return errors.New("node identity differs")
	}
	for _, expected := range []struct{ namespace, name, uid string }{
		{worker.GetWorkerNamespace(), worker.GetWorkerPod(), worker.GetWorkerPodUid()},
		{caller.Namespace, caller.PodName, caller.PodUID},
	} {
		pod, err := s.kube.CoreV1().Pods(expected.namespace).Get(ctx, expected.name, metav1.GetOptions{})
		if err != nil || string(pod.UID) != expected.uid || pod.Spec.NodeName != caller.NodeName || pod.DeletionTimestamp != nil {
			return errors.New("pod identity differs")
		}
	}
	return nil
}

func (s *Server) bootstrapClaims(actor *ateapi.Actor, caller *ateletauth.Caller, now time.Time) *actoridjwt.Claims {
	meta := actor.GetMetadata()
	activation := actor.GetStatus().GetSupervisorActivation()
	return &actoridjwt.Claims{
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
}
