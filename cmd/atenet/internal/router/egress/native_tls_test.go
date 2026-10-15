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
	"slices"
	"testing"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
	"github.com/agent-substrate/substrate/internal/egresspolicy"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

const nativeDestination = "native.example.test:8443"

func nativeActor() *ateapipb.Actor {
	a := runningActor()
	a.Metadata.Atespace, a.Metadata.Name = "foo", "bar"
	return a
}

func nativeIdentity(t *testing.T, c *x509.Certificate) {
	t.Helper()
	if err := substratex509.AddActorIdentityToCertificate(&substratex509.ActorIdentity{
		Atespace: "foo", ActorName: "bar", ActorUid: testEgressActorUID,
	}, c); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTLSDestination(t *testing.T) {
	if _, err := NativeTLSDestination(nativeDestination); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "native.example.test", "native.example.test:0", "*.example.test:8443", "127.0.0.1:8443", "[::1]:8443", "https://native.example.test:8443", "NATIVE.example.test:8443", "native.example.test.:8443", "native.example.test:08443"} {
		t.Run(value, func(t *testing.T) {
			if _, err := NativeTLSDestination(value); err == nil {
				t.Fatal("invalid destination accepted")
			}
		})
	}
}

func TestNativeTLSRequiresExactCurrentAuthority(t *testing.T) {
	ca := newTestCA(t, "native-actor-ca")
	for _, name := range []string{"allowed", "missing identity", "changed actor UID", "wrong actor", "not running", "missing policy", "wildcard", "all ports", "multiple ports", "multiple names", "https rule", "wrong policy port", "wrong destination port", "cleartext leg", "no leg"} {
		t.Run(name, func(t *testing.T) {
			client := &egressMockClient{actor: nativeActor(), policy: passthroughPolicy(ports(8443), "native.example.test")}
			options := actorCertOptions{mutate: func(c *x509.Certificate) { nativeIdentity(t, c) }}
			if name == "missing identity" {
				options.mutate = nil
			}
			leaf := ca.issueActorCert(t, "spiffe://substrate-actor.local/ateom-for-actor/foo/bar", options)
			md := egressMetadata(xfccHeader(leaf))
			md.Host = "192.0.2.1:8443"
			switch name {
			case "changed actor UID":
				client.actor.Metadata.Uid = "recreated-actor"
			case "wrong actor":
				client.actor.Metadata.Name = "different"
			case "not running":
				client.actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
			case "missing policy":
				client.policy = nil
			case "wildcard":
				client.policy = passthroughPolicy(ports(8443), "*.example.test")
			case "all ports":
				client.policy = passthroughPolicy(allPorts(), "native.example.test")
			case "multiple ports":
				client.policy = passthroughPolicy(ports(443, 8443), "native.example.test")
			case "multiple names":
				client.policy = passthroughPolicy(ports(8443), "native.example.test", "other.example.test")
			case "https rule":
				client.policy = httpsPolicyOnPorts(ports(8443), "native.example.test")
			case "wrong policy port":
				client.policy = passthroughPolicy(ports(443), "native.example.test")
			case "wrong destination port":
				md.Host = "192.0.2.1:443"
			case "cleartext leg":
				md.Attributes["envoy.filters.http.ext_proc"].Fields[extproc.FilterChainNameAttribute] = structpb.NewStringValue(extproc.EgressCleartextFilterChainName)
			}
			if name == "no leg" {
				md = agentgatewayEgressMetadata("")
			}
			h, err := NewNativeTLS(client, ca.roots(), nativeDestination)
			if err != nil {
				t.Fatal(err)
			}
			result, err := h.HandleRequestHeaders(context.Background(), md)
			if name != "allowed" {
				wantStatus(t, err, envoy_type.StatusCode_Forbidden)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(sniRulesOf(t, result), []egresspolicy.SNIRule{{Pattern: "native.example.test", Mode: egresspolicy.SNIModePassthrough}}) {
				t.Fatal("not exact passthrough")
			}
			if result.Target != "" || passthroughDestinationOf(result) != "" {
				t.Fatal("request-controlled upstream")
			}
			client.policy = nil
			_, err = h.HandleRequestHeaders(context.Background(), md)
			wantStatus(t, err, envoy_type.StatusCode_Forbidden)
			if client.policyCalls.Load() != 2 {
				t.Fatal("policy was cached")
			}
		})
	}
}
