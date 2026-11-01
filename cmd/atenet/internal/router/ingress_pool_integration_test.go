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

package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
)

// Every ingress CONNECT tunnel must reach main_internal with its own target
// actor and its own authority, however many are open at once. The CONNECT HCM
// and the main_internal cluster come from XdsServer, so the test runs the
// config that ships. Only the main_internal listener is a stub that echoes
// the filter state.
func TestIngressConnectTunnelsKeepTheirOwnTarget(t *testing.T) {
	binary := os.Getenv("SUBSTRATE_NATIVE_TLS_ENVOY")
	if binary == "" {
		t.Fatal("SUBSTRATE_NATIVE_TLS_ENVOY must name the built Envoy binary")
	}
	x := NewXdsServer(18000)
	raw := func(m proto.Message) json.RawMessage {
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	type tree = map[string]any
	const hcm = "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager"
	echo := func(name, state string) tree {
		return tree{"header": tree{"key": name, "value": "%FILTER_STATE(" + state + ":PLAIN)%"}}
	}
	port := freePort(t)
	config := tree{
		"bootstrap_extensions": []any{tree{"name": "envoy.bootstrap.internal_listener", "typed_config": tree{"@type": "type.googleapis.com/envoy.extensions.bootstrap.internal_listener.v3.InternalListener"}}},
		"static_resources": tree{
			"clusters": []any{raw(x.buildMainInternalCluster())},
			"listeners": []any{
				tree{
					"name":    "connect",
					"address": tree{"socket_address": tree{"address": "127.0.0.1", "port_value": port}},
					"filter_chains": []any{tree{"filters": []any{tree{
						"name":         "envoy.filters.network.http_connection_manager",
						"typed_config": raw(x.buildConnectTerminateHCM("connect")),
					}}}},
				},
				tree{
					"name":              MainInternalName,
					"internal_listener": tree{},
					"filter_chains": []any{tree{"filters": []any{tree{"name": "envoy.filters.network.http_connection_manager", "typed_config": tree{
						"@type":       hcm,
						"stat_prefix": "inner",
						"route_config": tree{"name": "inner", "virtual_hosts": []any{tree{"name": "inner", "domains": []any{"*"}, "routes": []any{tree{
							"match":           tree{"prefix": "/"},
							"direct_response": tree{"status": 200},
							"response_headers_to_add": []any{
								echo("x-target", extproc.TargetActorFilterStateKey),
								echo("x-authority", extproc.ConnectAuthorityFilterStateKey),
							},
						}}}}},
						"http_filters": []any{tree{"name": "envoy.filters.http.router", "typed_config": tree{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}},
					}}}}},
				},
			},
		},
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "envoy.json")
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
		target, authority string
		conn              net.Conn
		br                *bufio.Reader
	}
	var tunnels []*tunnel
	// Four actors, each on two ports, all open at once.
	for i := 0; i < 32; i++ {
		target := fmt.Sprintf("foo/actor-%d", i/2%4)
		authority := []string{"192.0.2.10:8080", "192.0.2.10:9090"}[i%2]
		c, err := net.DialTimeout("tcp", address, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nate-target-actor: %s\r\n\r\n", authority, authority, target)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("CONNECT %s: %v %v", authority, resp, err)
		}
		tunnels = append(tunnels, &tunnel{target, authority, c, br})
	}
	wrong := 0
	for _, tn := range tunnels {
		_, _ = fmt.Fprint(tn.conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(tn.br, nil)
		if err != nil {
			t.Fatal(err)
		}
		gotTarget, gotAuthority := resp.Header.Get("x-target"), resp.Header.Get("x-authority")
		if gotTarget != tn.target || gotAuthority != tn.authority {
			wrong++
			t.Errorf("tunnel for %s at %s reached main_internal as target %q, authority %q", tn.target, tn.authority, gotTarget, gotAuthority)
		}
		_ = tn.conn.Close()
	}
	t.Logf("%d of %d tunnels arrived with another tunnel's state", wrong, len(tunnels))
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
