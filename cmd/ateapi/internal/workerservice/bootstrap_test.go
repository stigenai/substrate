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
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/ateletauth/ateletauthtest"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/actoridjwt"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/protoredact"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/supervisorbootstrap"
	"go.ate.dev/substrate/internal/supervisorbootstrap/supervisorbootstraptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const activationID = "22222222-2222-4222-8222-222222222222"
const bootstrapWorkerUID = "33333333-3333-4333-8333-333333333333"

type bootstrapStore struct {
	store.Interface
	actor      *ateapi.Actor
	template   *ateapi.ActorTemplate
	worker     *ateapi.Worker
	assignment *ateapi.ActorAssignment
	reads      int
	changed    bool
}

func (s *bootstrapStore) GetActor(context.Context, resources.ActorRef) (*ateapi.Actor, error) {
	s.reads++
	a := proto.CloneOf(s.actor)
	if s.changed && s.reads > 1 {
		a.Metadata.Version++
	}
	return a, nil
}
func (s *bootstrapStore) GetActorTemplate(context.Context, resources.ActorTemplateRef) (*ateapi.ActorTemplate, error) {
	return s.template, nil
}
func (s *bootstrapStore) GetWorker(context.Context, string) (*ateapi.Worker, error) {
	return s.worker, nil
}
func (s *bootstrapStore) GetWorkerAssignment(context.Context, string, string) (*ateapi.ActorAssignment, error) {
	return s.assignment, nil
}

func bootstrapFixture(t *testing.T) (*Server, *bootstrapStore, *ateapi.MintSupervisorBootstrapRequest, context.Context) {
	t.Helper()
	template := supervisorbootstraptest.Template()
	activation, err := supervisorbootstrap.Describe(template)
	if err != nil {
		t.Fatal(err)
	}
	activation.ActivationId = activationID
	activation.WorkerUid = bootstrapWorkerUID
	_, actor := newFakeStoreWithActor("team-a", "my-actor", "3b9f1e77-2c4d-4a80-91be-6d5c8f0a7e21")
	actor.Status.SupervisorActivation = activation
	actor.Status.WorkerAssignment = &ateapi.WorkerAssignment{Worker: &ateapi.ObjectRef{Name: bootstrapWorkerUID}, WorkerPodUid: bootstrapWorkerUID, NodeName: "node-1"}
	worker := &ateapi.Worker{Metadata: &ateapi.ResourceMetadata{Name: bootstrapWorkerUID}, WorkerNamespace: "workers", WorkerPod: "worker-1", WorkerPodUid: bootstrapWorkerUID, NodeName: "node-1", Status: &ateapi.WorkerStatus{State: ateapi.WorkerState_WORKER_STATE_ACTIVE}}
	req := &ateapi.MintSupervisorBootstrapRequest{Actor: &ateapi.ObjectRef{Atespace: "team-a", Name: "my-actor"}, ActorUid: actor.Metadata.Uid, ActivationId: activationID, WorkerUid: bootstrapWorkerUID}
	st := &bootstrapStore{actor: actor, template: template, worker: worker, assignment: &ateapi.ActorAssignment{Actor: req.Actor, ActorUid: req.ActorUid}}
	identity := ateletauthtest.PodIdentityOn("node-1")
	kube := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", UID: types.UID(identity.NodeUID)}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: worker.WorkerPod, Namespace: worker.WorkerNamespace, UID: types.UID(bootstrapWorkerUID)}, Spec: corev1.PodSpec{NodeName: "node-1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: identity.PodName, Namespace: identity.Namespace, UID: types.UID(identity.PodUID)}, Spec: corev1.PodSpec{NodeName: "node-1"}})
	s := New(st, &fakeControl{}, testAteletSPIFFEID, nil, testActorJWTIssuer, newTestJWTPool(t), WithKubernetes(kube))
	return s, st, req, ateletauthtest.ContextWith(ateletauthtest.CertOn(t, "node-1"))
}

func TestMintSupervisorBootstrapSignedBinding(t *testing.T) {
	s, st, req, ctx := bootstrapFixture(t)
	response, err := s.MintSupervisorBootstrap(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(response.Token, ".")
	if len(parts) != 3 {
		t.Fatal("JWT format")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatal("signature encoding")
	}
	keys, err := s.actorIDJWTPool.VerificationKeys()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(keys[0].PublicKey.(*ecdsa.PublicKey), hash[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims actoridjwt.WireClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	var b *actoridjwt.SupervisorClaims
	if claims.Actor != nil {
		b = claims.Actor.Supervisor
	}
	if claims.Issuer != testActorJWTIssuer || claims.Subject != "actor/team-a/my-actor" || string(claims.Audiences) != `["urn:fabriek:native-bootstrap"]` || claims.Expiration-claims.IssuedAt != 300 || claims.NotBefore != claims.IssuedAt || claims.JTI == "" {
		t.Fatal("standard binding mismatch")
	}
	if claims.Actor == nil || claims.Actor.ActorUID != req.ActorUid || b == nil || b.SchemaVersion != supervisorbootstrap.Schema || b.Purpose != supervisorbootstrap.Purpose || b.ActivationID != activationID || b.WorkerUID != bootstrapWorkerUID || b.NodeUID != "node-uid" || b.TemplateDigest != st.actor.Status.SupervisorActivation.TemplateDigest || b.ImageDigest != st.actor.Status.SupervisorActivation.ImageDigest || b.LaunchDigest != st.actor.Status.SupervisorActivation.LaunchDigest {
		t.Fatal("activation binding mismatch")
	}
	redacted, changed := protoredact.Redacted(response)
	if !changed || redacted.(*ateapi.MintSupervisorBootstrapResponse).Token != protoredact.Placeholder || response.Token == protoredact.Placeholder {
		t.Fatal("response redaction failed")
	}
}

func TestMintSupervisorBootstrapDenied(t *testing.T) {
	cases := map[string]func(*Server, *bootstrapStore, *ateapi.MintSupervisorBootstrapRequest){
		"unconfigured": func(s *Server, _ *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) { s.kube = nil },
		"wrong actor": func(_ *Server, _ *bootstrapStore, r *ateapi.MintSupervisorBootstrapRequest) {
			r.ActorUid = activationID
		},
		"old generation": func(_ *Server, _ *bootstrapStore, r *ateapi.MintSupervisorBootstrapRequest) {
			r.ActivationId = bootstrapWorkerUID
		},
		"wrong worker": func(_ *Server, _ *bootstrapStore, r *ateapi.MintSupervisorBootstrapRequest) {
			r.WorkerUid = activationID
		},
		"wrong host": func(_ *Server, s *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) {
			s.actor.Status.WorkerAssignment.NodeName = "other"
		},
		"suspended": func(_ *Server, s *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) {
			s.actor.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
		},
		"golden": func(_ *Server, s *bootstrapStore, r *ateapi.MintSupervisorBootstrapRequest) {
			// The template reconciler names the golden actor after the template UID.
			s.actor.Metadata.Name = s.template.Metadata.Uid
			r.Actor.Name = s.template.Metadata.Uid
		},
		"template changed": func(_ *Server, s *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) {
			s.template.Containers[0].Env[0].Value = "sha256:" + strings.Repeat("c", 64)
		},
		"actor transitioned": func(_ *Server, s *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) { s.changed = true },
		"reassigned": func(_ *Server, s *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) {
			s.assignment.ActorUid = activationID
		},
		"dead atelet": func(s *Server, _ *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) {
			id := ateletauthtest.PodIdentityOn("node-1")
			_ = s.kube.CoreV1().Pods(id.Namespace).Delete(context.Background(), id.PodName, metav1.DeleteOptions{})
		},
		"dead worker": func(s *Server, st *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) {
			_ = s.kube.CoreV1().Pods(st.worker.WorkerNamespace).Delete(context.Background(), st.worker.WorkerPod, metav1.DeleteOptions{})
		},
		"node recreated": func(s *Server, _ *bootstrapStore, _ *ateapi.MintSupervisorBootstrapRequest) {
			n, _ := s.kube.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
			n.UID = "recreated"
			_, _ = s.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, st, req, ctx := bootstrapFixture(t)
			mutate(s, st, req)
			resp, err := s.MintSupervisorBootstrap(ctx, req)
			if resp != nil || apierror.Code(err) != codes.PermissionDenied {
				t.Fatalf("denial: %v", err)
			}
		})
	}
	t.Run("unauthenticated", func(t *testing.T) {
		s, _, req, _ := bootstrapFixture(t)
		if _, err := s.MintSupervisorBootstrap(context.Background(), req); apierror.Code(err) != codes.Unauthenticated {
			t.Fatal(err)
		}
	})
}
