//go:build native_tls_integration

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
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// This explicit integration tier runs an actual Envoy binary with the built
// dynamic module. Missing binaries fail; ordinary unit tests do not invoke it.
// Only Control's actor/policy data is synthetic. Both TLS legs, XFCC sanitation,
// ext_proc, policy metadata, SNI filter and fixed TCP cluster are real.
func TestNativeTLSEnvoy(t *testing.T) {
	binary := os.Getenv("SUBSTRATE_NATIVE_TLS_ENVOY")
	if binary == "" {
		t.Fatal("SUBSTRATE_NATIVE_TLS_ENVOY must name the built Envoy binary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	ca := newTestCA(t, "gateway-ca")
	originCA := newTestCA(t, "origin-ca")
	actorCert := nativeTLSCert(t, ca, "localhost", &substratex509.ActorIdentity{Atespace: "foo", ActorName: "bar", ActorUid: testEgressActorUID})
	gatewayCert := nativeTLSCert(t, ca, "localhost", nil)
	originCert := nativeTLSCert(t, originCA, "localhost", nil)
	var hits atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = io.WriteString(w, "pinned origin") }))
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{originCert}, MinVersion: tls.VersionTLS12}
	origin.StartTLS()
	defer origin.Close()
	_, portText, err := net.SplitHostPort(origin.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	client := &nativeTLSControl{actor: nativeActor(), policy: passthroughPolicy(ports(int32(port)), "localhost")}
	handler, err := NewNativeTLS(client, ca.roots(), net.JoinHostPort("localhost", portText))
	if err != nil {
		t.Fatal(err)
	}
	metric, err := extproc.NewRouteDurationHistogram()
	if err != nil {
		t.Fatal(err)
	}
	server := extproc.NewServer(0, metric, extproc.Handlers{extproc.DirectionEgress: handler}).NewGRPCServer()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer server.Stop()
	go func() { _ = server.Serve(listener) }()
	dir := t.TempDir()
	writeTLSBundle(t, dir, "gateway", gatewayCert)
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	gatewayPort := freeNativeTLSPort(t)
	_, extPort, _ := net.SplitHostPort(listener.Addr().String())
	config := fmt.Sprintf(nativeTLSEnvoyConfig, gatewayPort, dir, dir, dir, extPort, port)
	configPath := filepath.Join(dir, "envoy.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "-c", configPath, "--disable-hot-restart", "--concurrency", "1", "--log-level", "error")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
		if t.Failed() {
			t.Log(output.String())
		}
	}()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(gatewayPort))
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, e := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if e == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Envoy did not listen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cases := []string{"allowed", "connect IP cannot redirect", "wrong actor", "recreated UID", "stopped actor", "revoked policy", "broad policy", "wrong SNI", "missing SNI", "wrong port", "cleartext", "forged XFCC"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			client.set(nativeActor(), passthroughPolicy(ports(int32(port)), "localhost"))
			cert := actorCert
			target := net.JoinHostPort("127.0.0.1", portText)
			sni := "localhost"
			switch name {
			case "connect IP cannot redirect":
				target = net.JoinHostPort("192.0.2.99", portText)
			case "wrong actor", "forged XFCC":
				cert = nativeTLSCert(t, ca, "localhost", &substratex509.ActorIdentity{Atespace: "foo", ActorName: "other", ActorUid: testEgressActorUID})
			case "recreated UID":
				a := nativeActor()
				a.Metadata.Uid = "replacement"
				client.set(a, passthroughPolicy(ports(int32(port)), "localhost"))
			case "stopped actor":
				a := nativeActor()
				a.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
				client.set(a, passthroughPolicy(ports(int32(port)), "localhost"))
			case "revoked policy":
				client.set(nativeActor(), nil)
			case "broad policy":
				client.set(nativeActor(), passthroughPolicy(allPorts(), "*"))
			case "wrong SNI":
				sni = "other.example.test"
			case "missing SNI":
				sni = ""
			case "wrong port":
				target = net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1))
			}
			before := hits.Load()
			outer, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address, &tls.Config{RootCAs: ca.roots(), ServerName: "localhost", Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			defer outer.Close()
			_ = outer.SetDeadline(time.Now().Add(3 * time.Second))
			header := ""
			if name == "forged XFCC" {
				leaf, _ := x509.ParseCertificate(actorCert.Certificate[0])
				header = "X-Forwarded-Client-Cert: " + xfccHeader(leaf) + "\r\n"
			}
			_, err = fmt.Fprintf(outer, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, header)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(outer), &http.Request{Method: "CONNECT"})
			if err != nil {
				t.Fatal(err)
			}
			allowed := name == "allowed" || name == "connect IP cannot redirect"
			if response.StatusCode != http.StatusOK {
				if allowed {
					t.Fatalf("CONNECT denied: %d", response.StatusCode)
				}
				if hits.Load() != before {
					t.Fatal("denied CONNECT reached origin")
				}
				return
			}
			if name == "cleartext" {
				_, _ = io.WriteString(outer, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
				var b [1]byte
				if n, e := outer.Read(b[:]); n != 0 || e == nil {
					t.Fatal("cleartext was served")
				}
				if hits.Load() != before {
					t.Fatal("cleartext reached origin")
				}
				return
			}
			inner := tls.Client(outer, &tls.Config{RootCAs: originCA.roots(), ServerName: sni, MinVersion: tls.VersionTLS12, InsecureSkipVerify: sni == ""}) // Missing SNI is a negative probe only.
			err = inner.HandshakeContext(ctx)
			if !allowed {
				if err == nil {
					t.Fatal("denied TLS completed")
				}
				if hits.Load() != before {
					t.Fatal("denied TLS reached origin")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(inner.ConnectionState().PeerCertificates[0].Raw, originCert.Certificate[0]) {
				t.Fatal("origin certificate was replaced")
			}
			_, err = io.WriteString(inner, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			res, err := http.ReadResponse(bufio.NewReader(inner), &http.Request{Method: "GET"})
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil || string(body) != "pinned origin" {
				t.Fatalf("origin response: %q %v", body, err)
			}
			if hits.Load() != before+1 {
				t.Fatal("not exactly one origin request")
			}
		})
	}
}

type nativeTLSControl struct {
	ateapipb.ControlClient
	mu     sync.Mutex
	actor  *ateapipb.Actor
	policy *ateapipb.EgressPolicy
}

func (c *nativeTLSControl) set(a *ateapipb.Actor, p *ateapipb.EgressPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actor = a
	c.policy = p
}
func (c *nativeTLSControl) GetActor(_ context.Context, r *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Actor.Name != c.actor.Metadata.Name || r.Actor.Atespace != c.actor.Metadata.Atespace {
		return nil, status.Error(codes.NotFound, "absent")
	}
	return proto.Clone(c.actor).(*ateapipb.Actor), nil
}
func (c *nativeTLSControl) GetActorEgressPolicy(context.Context, *ateapipb.GetActorEgressPolicyRequest, ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.policy == nil {
		return nil, status.Error(codes.NotFound, "absent")
	}
	return proto.Clone(c.policy).(*ateapipb.EgressPolicy), nil
}
func freeNativeTLSPort(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func nativeTLSCert(t *testing.T, ca *testCA, host string, identity *substratex509.ActorIdentity) tls.Certificate {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if e != nil {
		t.Fatal(e)
	}
	c := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	if identity != nil {
		u, _ := url.Parse("spiffe://substrate-actor.local/ateom-for-actor/" + identity.Atespace + "/" + identity.ActorName)
		c.URIs = []*url.URL{u}
		if e := substratex509.AddActorIdentityToCertificate(identity, c); e != nil {
			t.Fatal(e)
		}
	}
	der, e := x509.CreateCertificate(rand.Reader, c, ca.cert, &key.PublicKey, ca.key)
	if e != nil {
		t.Fatal(e)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
func writeTLSBundle(t *testing.T, dir, name string, c tls.Certificate) {
	t.Helper()
	key, e := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	for suffix, b := range map[string][]byte{".crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), ".key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
		if e := os.WriteFile(filepath.Join(dir, name+suffix), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
}

const nativeTLSEnvoyConfig = `node:
  id: atenet-egress
  cluster: atenet-egress
bootstrap_extensions:
- name: envoy.bootstrap.internal_listener
  typed_config:
    '@type': type.googleapis.com/envoy.extensions.bootstrap.internal_listener.v3.InternalListener
static_resources:
  listeners:
  - name: egress
    address:
      socket_address:
        address: 127.0.0.1
        port_value: %d
    filter_chains:
    - name: egress
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
          require_client_certificate: true
          common_tls_context:
            validation_context:
              trusted_ca:
                filename: %s/ca.pem
            tls_certificates:
            - certificate_chain:
                filename: %s/gateway.crt
              private_key:
                filename: %s/gateway.key
      filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: egress_connect
          codec_type: HTTP1
          upgrade_configs:
          - upgrade_type: CONNECT
          forward_client_cert_details: SANITIZE_SET
          set_current_client_cert_details:
            chain: true
          route_config:
            name: connect_route
            virtual_hosts:
            - name: connect
              domains:
              - '*'
              routes:
              - match:
                  connect_matcher: {}
                route:
                  cluster: mitm_internal
                  timeout: 0s
                  upgrade_configs:
                  - upgrade_type: CONNECT
                    connect_config: {}
          http_filters:
          - name: envoy.filters.http.set_filter_state
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.set_filter_state.v3.Config
              on_request_headers:
              - object_key: dev.ate.actor.identity
                factory_key: envoy.string
                shared_with_upstream: ONCE
                skip_if_empty: true
                format_string:
                  text_format_source:
                    inline_string: '%%DOWNSTREAM_PEER_URI_SAN%%'
                  omit_empty_values: true
              - object_key: dev.ate.connect.authority
                factory_key: envoy.string
                shared_with_upstream: ONCE
                skip_if_empty: true
                format_string:
                  text_format_source:
                    inline_string: '%%REQ(:AUTHORITY)%%'
                  omit_empty_values: true
              - object_key: envoy.network.upstream_server_name
                skip_if_empty: true
                format_string:
                  text_format_source:
                    inline_string: '%%DOWNSTREAM_PEER_URI_SAN%%'
                  omit_empty_values: true
          - name: envoy.filters.http.ext_proc
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
              stat_prefix: egress_identity
              grpc_service:
                envoy_grpc:
                  cluster_name: ext_proc_server
                timeout: 2s
              failure_mode_allow: false
              message_timeout: 5s
              request_attributes:
              - xds.filter_chain_name
              processing_mode:
                request_header_mode: SEND
                response_header_mode: SKIP
                request_body_mode: NONE
                response_body_mode: NONE
                request_trailer_mode: SKIP
                response_trailer_mode: SKIP
              mutation_rules:
                disallow_system: true
                disallow_is_error: true
              metadata_options:
                receiving_namespaces:
                  untyped:
                  - dev.ate.egress
                  - dev.ate.policy.egress
          - name: envoy.filters.http.set_filter_state
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.set_filter_state.v3.Config
              on_request_headers:
              - object_key: dev.ate.policy.egress
                factory_key: envoy.string
                skip_if_empty: true
                shared_with_upstream: ONCE
                format_string:
                  text_format_source:
                    inline_string: '%%DYNAMIC_METADATA(dev.ate.policy.egress)%%'
                  omit_empty_values: true
          - name: envoy.filters.http.router
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  - name: mitm_listener
    stat_prefix: mitm
    internal_listener: {}
    listener_filters_timeout: 1s
    continue_on_listener_filters_timeout: true
    listener_filters:
    - name: envoy.filters.listener.tls_inspector
      typed_config:
        '@type': type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector
    - name: envoy.filters.listener.http_inspector
      typed_config:
        '@type': type.googleapis.com/envoy.extensions.filters.listener.http_inspector.v3.HttpInspector
    - name: envoy.filters.listener.dynamic_modules
      typed_config:
        '@type': type.googleapis.com/envoy.extensions.filters.listener.dynamic_modules.v3.DynamicModuleListenerFilter
        dynamic_module_config:
          name: envoy_substrate_egress_policy
        filter_name: envoy_substrate_egress_policy
    filter_chain_matcher:
      matcher_tree:
        input:
          name: filter_state
          typed_config:
            '@type': type.googleapis.com/envoy.extensions.matching.common_inputs.network.v3.FilterStateInput
            key: dev.ate.egress.filter_chain
        exact_match_map:
          map:
            passthrough:
              action:
                name: native_tls
                typed_config:
                  '@type': type.googleapis.com/google.protobuf.StringValue
                  value: native_tls
    filter_chains:
    - name: native_tls
      filters:
      - name: envoy.filters.network.tcp_proxy
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
          stat_prefix: native_tls
          cluster: native_tls
  clusters:
  - name: mitm_internal
    connect_timeout: 1s
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        '@type': type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        common_http_protocol_options:
          max_requests_per_connection: 1
        explicit_http_config:
          http_protocol_options: {}
    transport_socket:
      name: envoy.transport_sockets.internal_upstream
      typed_config:
        '@type': type.googleapis.com/envoy.extensions.transport_sockets.internal_upstream.v3.InternalUpstreamTransport
        transport_socket:
          name: envoy.transport_sockets.raw_buffer
          typed_config:
            '@type': type.googleapis.com/envoy.extensions.transport_sockets.raw_buffer.v3.RawBuffer
    load_assignment:
      cluster_name: mitm_internal
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              envoy_internal_address:
                server_listener_name: mitm_listener
  - name: ext_proc_server
    type: STATIC
    lb_policy: ROUND_ROBIN
    connect_timeout: 1s
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        '@type': type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    load_assignment:
      cluster_name: ext_proc_server
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: 127.0.0.1
                port_value: %s
  - name: native_tls
    type: STRICT_DNS
    dns_lookup_family: V4_ONLY
    connect_timeout: 1s
    load_assignment:
      cluster_name: native_tls
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: localhost
                port_value: %d
`
