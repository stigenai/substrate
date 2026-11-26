//go:build native_tls_integration

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

package egress

import (
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// Envoy keeps one upstream TLS session cache per cluster and offers its newest
// session to any SNI. This test runs the manifest's upstream TLS clusters
// against one origin that serves two hosts and accepts a session ticket across
// SNI, as Cloudflare does. Calls to the two hosts alternate, and each must
// reach the host its Host header names.
//
// The container must resolve api.test and chat.test to 127.0.0.1
// (hack/test-native-tls.sh passes --add-host). SUBSTRATE_EGRESS_MANIFEST names
// the egress manifest.
func TestEgressUpstreamSessionPerSNI(t *testing.T) {
	binary := os.Getenv("SUBSTRATE_NATIVE_TLS_ENVOY")
	manifestPath := os.Getenv("SUBSTRATE_EGRESS_MANIFEST")
	if binary == "" || manifestPath == "" {
		t.Fatal("SUBSTRATE_NATIVE_TLS_ENVOY and SUBSTRATE_EGRESS_MANIFEST must be set")
	}
	ctx := t.Context()

	ca := newTestCA(t, "origin-ca")
	certs := map[string]tls.Certificate{
		"api.test":  nativeTLSCert(t, ca, "api.test", nil),
		"chat.test": nativeTLSCert(t, ca, "chat.test", nil),
	}
	var mu sync.Mutex
	var served []string
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		mu.Lock()
		if r.TLS.ServerName != host {
			served = append(served, fmt.Sprintf("Host %s over SNI %s", host, r.TLS.ServerName))
		}
		mu.Unlock()
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, host)
	}))
	origin.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, ok := certs[hello.ServerName]
			if !ok {
				return nil, fmt.Errorf("unknown SNI %q", hello.ServerName)
			}
			return &c, nil
		},
	}
	origin.StartTLS()
	defer origin.Close()
	_, originPort, _ := net.SplitHostPort(origin.Listener.Addr().String())

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	gatewayPort := freeNativeTLSPort(t)
	configPath := filepath.Join(dir, "envoy.yaml")
	if err := os.WriteFile(configPath, upstreamSessionConfig(t, manifestPath, caPath, gatewayPort, originPort), 0600); err != nil {
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
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Envoy did not listen")
		}
	}

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for _, cluster := range upstreamTLSClusters {
		t.Run(cluster, func(t *testing.T) {
			for i := range 20 {
				host := []string{"chat.test", "api.test"}[i%2]
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = net.JoinHostPort(host, originPort)
				req.Header.Set("x-host", host)
				req.Header.Set("x-cluster", cluster)
				res, err := client.Do(req)
				if err != nil {
					t.Fatalf("call %d to %s: %v", i, host, err)
				}
				body, _ := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if res.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != host {
					t.Fatalf("call %d to %s: status %d, body %q", i, host, res.StatusCode, body)
				}
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(served) > 0 {
		t.Errorf("origin saw a Host on another host's SNI: %v", served)
	}
}

// upstreamTLSClusters are the manifest clusters that re-originate TLS with the
// SNI taken from the dialed name.
var upstreamTLSClusters = []string{"egress_forward_proxy", "egress_forward_proxy_grpc"}

// upstreamSessionConfig builds an Envoy bootstrap with one plain HTTP listener
// that routes by x-cluster to the manifest's upstream TLS clusters. The
// clusters are the manifest's own, except for the trusted CA and the DNS
// cache's resolved_address_filter, which would refuse loopback.
func upstreamSessionConfig(t *testing.T, manifestPath, caPath string, gatewayPort int, originPort string) []byte {
	t.Helper()
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]any
	for _, doc := range strings.Split(string(manifest), "\n---\n") {
		var obj struct {
			Kind string            `json:"kind"`
			Data map[string]string `json:"data"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind == "ConfigMap" && obj.Data["envoy.yaml"] != "" {
			if err := yaml.Unmarshal([]byte(obj.Data["envoy.yaml"]), &source); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if source == nil {
		t.Fatalf("%s has no envoy.yaml", manifestPath)
	}
	var picked []any
	var dnsCache map[string]any
	for _, c := range source["static_resources"].(map[string]any)["clusters"].([]any) {
		cluster := c.(map[string]any)
		if name, _ := cluster["name"].(string); !contains(upstreamTLSClusters, name) {
			continue
		}
		tlsContext := cluster["transport_socket"].(map[string]any)["typed_config"].(map[string]any)["common_tls_context"].(map[string]any)
		tlsContext["validation_context"].(map[string]any)["trusted_ca"] = map[string]any{"filename": caPath}
		dnsCache = cluster["cluster_type"].(map[string]any)["typed_config"].(map[string]any)["dns_cache_config"].(map[string]any)
		delete(dnsCache, "resolved_address_filter")
		picked = append(picked, cluster)
	}
	if len(picked) != len(upstreamTLSClusters) {
		t.Fatalf("found %d of the %d upstream TLS clusters in the manifest", len(picked), len(upstreamTLSClusters))
	}
	var harness map[string]any
	if err := yaml.Unmarshal([]byte(fmt.Sprintf(upstreamSessionHarness, gatewayPort, originPort)), &harness); err != nil {
		t.Fatal(err)
	}
	listeners := harness["static_resources"].(map[string]any)["listeners"].([]any)
	filters := listeners[0].(map[string]any)["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)
	httpFilters := filters[0].(map[string]any)["typed_config"].(map[string]any)["http_filters"].([]any)
	forwardProxy := httpFilters[1].(map[string]any)["typed_config"].(map[string]any)
	forwardProxy["dns_cache_config"] = dnsCache
	harness["static_resources"].(map[string]any)["clusters"] = picked
	out, err := yaml.Marshal(harness)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

const upstreamSessionHarness = `static_resources:
  listeners:
  - name: harness
    address:
      socket_address:
        address: 127.0.0.1
        port_value: %d
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: harness
          route_config:
            virtual_hosts:
            - name: all
              domains: ['*']
              routes:
              - match:
                  prefix: /
                route:
                  cluster_header: x-cluster
          http_filters:
          - name: envoy.filters.http.set_filter_state
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.set_filter_state.v3.Config
              on_request_headers:
              - object_key: envoy.upstream.dynamic_host
                format_string:
                  text_format_source:
                    inline_string: '%%REQ(x-host)%%'
              - object_key: envoy.upstream.dynamic_port
                format_string:
                  text_format_source:
                    inline_string: '%s'
          - name: envoy.filters.http.dynamic_forward_proxy
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.dynamic_forward_proxy.v3.FilterConfig
              allow_dynamic_host_from_filter_state: true
          - name: envoy.filters.http.router
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
`
