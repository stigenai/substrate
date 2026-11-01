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
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/agent-substrate/substrate/internal/substratex509"
)

type tree = map[string]any

// poolManifests are the egress installs; each must pass the same test.
var poolManifests = []string{"atenet-egress.yaml", "atenet-egress-with-sdsmint.yaml"}

// manifestEnvoy returns the envoy.yaml of the atenet-egress ConfigMap in path.
func manifestEnvoy(t *testing.T, path string) tree {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind == "ConfigMap" && obj.Metadata.Name == "atenet-egress" {
			var out tree
			if err := yaml.Unmarshal([]byte(obj.Data["envoy.yaml"]), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no atenet-egress ConfigMap in %s", path)
	return nil
}

// at walks a generic tree by map keys and list indexes, failing on a gap.
func at(t *testing.T, n any, keys ...any) any {
	t.Helper()
	for _, k := range keys {
		switch kk := k.(type) {
		case string:
			m, ok := n.(tree)
			if !ok || m[kk] == nil {
				t.Fatalf("manifest has no %q", kk)
			}
			n = m[kk]
		case int:
			l, ok := n.([]any)
			if !ok || kk >= len(l) {
				t.Fatalf("manifest has no index %d", kk)
			}
			n = l[kk]
		}
	}
	return n
}

// Every tunnel an actor opens must reach the inner listener with its own CONNECT
// authority and its own identity, however many are open at once. The
// set_filter_state filter and the inner pool cluster come from the manifest,
// not from a copy.
func TestEgressConnectAuthorityPerTunnel(t *testing.T) {
	binary := os.Getenv("SUBSTRATE_NATIVE_TLS_ENVOY")
	if binary == "" {
		t.Fatal("SUBSTRATE_NATIVE_TLS_ENVOY must name the built Envoy binary")
	}
	dir := os.Getenv("SUBSTRATE_EGRESS_MANIFESTS")
	if dir == "" {
		t.Fatal("SUBSTRATE_EGRESS_MANIFESTS must name the manifests/ate-install directory")
	}
	for _, name := range poolManifests {
		t.Run(name, func(t *testing.T) { runConnectPool(t, binary, filepath.Join(dir, name)) })
	}
}

// pool returns the egress listener's first set_filter_state filter and the
// cluster its CONNECT route sends tunnels to, with the name of the internal
// listener that cluster dials.
func pool(t *testing.T, m tree) (setState, cluster any, listener string) {
	t.Helper()
	var name string
	for _, l := range at(t, m, "static_resources", "listeners").([]any) {
		if l.(tree)["name"] != "egress" {
			continue
		}
		hcm := at(t, l, "filter_chains", 0, "filters", 0, "typed_config")
		name = at(t, hcm, "route_config", "virtual_hosts", 0, "routes", 0, "route", "cluster").(string)
		for _, f := range at(t, hcm, "http_filters").([]any) {
			if f.(tree)["name"] == "envoy.filters.http.set_filter_state" {
				setState = f
				break
			}
		}
	}
	for _, c := range at(t, m, "static_resources", "clusters").([]any) {
		if c.(tree)["name"] == name {
			cluster = c
		}
	}
	if setState == nil || cluster == nil {
		t.Fatal("manifest lacks the first set_filter_state filter or the CONNECT route's cluster")
	}
	listener = at(t, cluster, "load_assignment", "endpoints", 0, "lb_endpoints", 0, "endpoint", "address", "envoy_internal_address", "server_listener_name").(string)
	return setState, cluster, listener
}

func connectPoolConfig(setState, cluster any, clusterName, listener, dir string, port int) tree {
	header := func(name, state string) tree {
		return tree{"header": tree{"key": name, "value": "%FILTER_STATE(" + state + ":PLAIN)%"}}
	}
	hcm := "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager"
	router := tree{"name": "envoy.filters.http.router", "typed_config": tree{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}
	return tree{
		"bootstrap_extensions": []any{tree{"name": "envoy.bootstrap.internal_listener", "typed_config": tree{"@type": "type.googleapis.com/envoy.extensions.bootstrap.internal_listener.v3.InternalListener"}}},
		"static_resources": tree{
			"clusters": []any{cluster},
			"listeners": []any{
				tree{
					"name":    "egress",
					"address": tree{"socket_address": tree{"address": "127.0.0.1", "port_value": port}},
					"filter_chains": []any{tree{
						"name": "egress",
						"transport_socket": tree{"name": "envoy.transport_sockets.tls", "typed_config": tree{
							"@type":                      "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
							"require_client_certificate": true,
							"common_tls_context": tree{
								"validation_context": tree{"trusted_ca": tree{"filename": dir + "/ca.pem"}},
								"tls_certificates":   []any{tree{"certificate_chain": tree{"filename": dir + "/gateway.crt"}, "private_key": tree{"filename": dir + "/gateway.key"}}},
							},
						}},
						"filters": []any{tree{"name": "envoy.filters.network.http_connection_manager", "typed_config": tree{
							"@type":           hcm,
							"stat_prefix":     "egress_connect",
							"codec_type":      "HTTP1",
							"upgrade_configs": []any{tree{"upgrade_type": "CONNECT"}},
							"route_config": tree{"name": "connect_route", "virtual_hosts": []any{tree{"name": "connect", "domains": []any{"*"}, "routes": []any{tree{
								"match": tree{"connect_matcher": tree{}},
								"route": tree{"cluster": clusterName, "timeout": "0s", "upgrade_configs": []any{tree{"upgrade_type": "CONNECT", "connect_config": tree{}}}},
							}}}}},
							"http_filters": []any{setState, router},
						}}},
					}},
				},
				tree{
					"name":              listener,
					"internal_listener": tree{},
					"filter_chains": []any{tree{"filters": []any{tree{"name": "envoy.filters.network.http_connection_manager", "typed_config": tree{
						"@type":       hcm,
						"stat_prefix": "inner",
						"route_config": tree{"name": "inner", "virtual_hosts": []any{tree{"name": "inner", "domains": []any{"*"}, "routes": []any{tree{
							"match":                   tree{"prefix": "/"},
							"direct_response":         tree{"status": 200},
							"response_headers_to_add": []any{header("x-connect-authority", "dev.ate.connect.authority"), header("x-actor", "dev.ate.actor.identity")},
						}}}}},
						"http_filters": []any{router},
					}}}}},
				},
			},
		},
	}
}

func runConnectPool(t *testing.T, binary, path string) {
	setState, cluster, listener := pool(t, manifestEnvoy(t, path))
	ca := newTestCA(t, "actor-ca")
	dir := t.TempDir()
	writeTLSBundle(t, dir, "gateway", nativeTLSCert(t, ca, "localhost", nil))
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	actors := map[string]tls.Certificate{}
	for _, name := range []string{"a", "b"} {
		actors[name] = nativeTLSCert(t, ca, "localhost", &substratex509.ActorIdentity{Atespace: "foo", ActorName: name, ActorUid: testEgressActorUID})
	}
	port := freeNativeTLSPort(t)
	body, err := json.Marshal(connectPoolConfig(setState, cluster, cluster.(tree)["name"].(string), listener, dir, port))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "envoy.json")
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "-c", configPath, "--disable-hot-restart", "--concurrency", "1", "--log-level", "error")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
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
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, e := net.DialTimeout("tcp", address, 100*time.Millisecond); e == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Envoy did not listen: %s", output.String())
		}
	}

	type tunnel struct {
		actor, authority string
		conn             *tls.Conn
		br               *bufio.Reader
	}
	var tunnels []*tunnel
	open := func(actor, authority string) {
		c, err := tls.Dial("tcp", address, &tls.Config{Certificates: []tls.Certificate{actors[actor]}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // the test gateway cert is self-issued
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("CONNECT %s: %v %v", authority, resp, err)
		}
		tunnels = append(tunnels, &tunnel{actor, authority, c, br})
	}
	// Actor a alternates between two ports; b dials one. All stay open.
	for i := 0; i < 32; i++ {
		open("a", []string{"192.0.2.10:8080", "192.0.2.10:443"}[i%2])
	}
	for i := 0; i < 8; i++ {
		open("b", "192.0.2.10:8080")
	}
	wrong := 0
	for _, tn := range tunnels {
		_, _ = fmt.Fprint(tn.conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(tn.br, nil)
		if err != nil {
			t.Fatal(err)
		}
		gotAuthority, gotActor := resp.Header.Get("x-connect-authority"), resp.Header.Get("x-actor")
		if gotAuthority != tn.authority || !strings.HasSuffix(gotActor, "/"+tn.actor) {
			wrong++
			t.Errorf("actor %s tunnel to %s reached the inner listener as authority %q, actor %q", tn.actor, tn.authority, gotAuthority, gotActor)
		}
		_ = tn.conn.Close()
	}
	t.Logf("%d of %d tunnels arrived with another tunnel's state", wrong, len(tunnels))
}
