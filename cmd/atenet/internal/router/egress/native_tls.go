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

package egress

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"strconv"

	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
	"github.com/agent-substrate/substrate/internal/egresspolicy"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// NativeTLSDestination parses a canonical DNS hostname and explicit port. IP
// literals, URL syntax, wildcards, and implicit ports cannot become upstreams.
func NativeTLSDestination(address string) (egresspolicy.Destination, error) {
	dest, err := egresspolicy.NormalizeAuthority(address)
	if err != nil || dest.Hostname == "" || dest.IP.IsValid() || dest.Port == 0 ||
		net.JoinHostPort(dest.Hostname, strconv.Itoa(int(dest.Port))) != address {
		return egresspolicy.Destination{}, fmt.Errorf("--native-tls-destination requires a canonical DNS hostname:port")
	}
	return dest, nil
}

// NewNativeTLS serves one pinned native TLS destination only. Every CONNECT
// rereads the actor and policy; there is no MITM credential provider or cache.
// The private Envoy listener must map passthrough to the same fixed upstream.
func NewNativeTLS(api ateapipb.ControlClient, roots *x509.CertPool, address string) (*Handler, error) {
	dest, err := NativeTLSDestination(address)
	if err != nil {
		return nil, err
	}
	h := New(api, roots, 0, nil, "")
	h.nativeTLS = &dest
	return h, nil
}

// validateNativeActor binds the already verified peer to the current immutable
// UID, after the policy lookup. A name reused since certificate issuance denies.
func (h *Handler) validateNativeActor(ctx context.Context, md *extproc.RequestMetadata, ref resources.ActorRef) error {
	if md.Attribute(agentgatewayClientCertificateAttribute) != "" {
		return extproc.NewReqError(envoy_type.StatusCode_Forbidden, deniedBody)
	}
	chain, err := parseXFCCChain(md.Header(forwardedClientCertHeader))
	if err != nil {
		return extproc.NewReqError(envoy_type.StatusCode_Forbidden, deniedBody)
	}
	identity, err := substratex509.ActorIdentityFromCertificate(chain[0])
	if err != nil || identity == nil || identity.Atespace != ref.Atespace || identity.ActorName != ref.Name {
		return extproc.NewReqError(envoy_type.StatusCode_Forbidden, deniedBody)
	}
	actor, err := h.apiClient.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref.ToObjectRef()})
	if err != nil {
		return mapEgressIdentityError(ref.Atespace, ref.Name, err)
	}
	if actor.GetMetadata().GetUid() != identity.ActorUid ||
		resources.ActorRefFromActor(actor) != ref ||
		actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return extproc.NewReqError(envoy_type.StatusCode_Forbidden, deniedBody)
	}
	return nil
}
