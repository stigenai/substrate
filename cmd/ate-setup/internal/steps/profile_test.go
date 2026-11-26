// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package steps

import (
	"strings"
	"testing"

	"go.ate.dev/substrate/cmd/ate-setup/internal/config"
)

// A Kind-profile install renders the overlay ATE_INSTALL_PROFILE names, for the
// bundle and for every single-component redeploy.
func TestProfileSelectsTheOverlay(t *testing.T) {
	e := &Env{Cfg: &config.Config{
		Root: "/repo", Kind: true, ProfileDir: "k0s", Router: config.RouterEnvoy,
	}}
	for name, got := range map[string]string{
		"bundle":     SystemOverlay(e.Cfg),
		"atelet":     e.ateletManifestPath(),
		"api server": e.apiServerManifestPath(),
		"postgres":   e.postgresManifestPath(),
		"otel":       e.otelConfigPath(),
	} {
		if !strings.Contains(got, "/k0s") {
			t.Errorf("%s path = %q, want it under the k0s profile", name, got)
		}
	}
}

// The k0s profile keeps the kubelet root at /var/lib/k0s/kubelet but not the
// device plugin socket, drops the node credential provider that k0s lacks, and
// reads the S3 keys from a Secret.
func TestRenderK0sAtelet(t *testing.T) {
	cfg := &config.Config{Root: repoRoot(t), Kind: true, ProfileDir: "k0s"}
	e := &Env{Cfg: cfg}
	rendered, err := e.render(e.ateletManifestPath())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	daemonSets, _ := dataPlaneWorkloads(t, rendered)
	ds, ok := daemonSets["atelet"]
	if !ok {
		t.Fatal("rendered manifest has no DaemonSet atelet")
	}
	pod := ds.Spec.Template.Spec

	hostPaths := map[string]string{}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			hostPaths[v.Name] = v.HostPath.Path
		}
		if strings.HasPrefix(v.Name, "image-credential-provider") {
			t.Errorf("volume %s survived; k0s has no kubelet credential provider", v.Name)
		}
	}
	if got, want := hostPaths["kubelet-plugins"], "/var/lib/k0s/kubelet/plugins"; got != want {
		t.Errorf("kubelet-plugins hostPath = %q, want %q", got, want)
	}
	if got, want := hostPaths["device-plugins"], "/var/lib/kubelet/device-plugins"; got != want {
		t.Errorf("device-plugins hostPath = %q, want %q", got, want)
	}

	var atelet, plugin bool
	for _, c := range pod.Containers {
		if c.Name != "atelet" {
			continue
		}
		atelet = true
		for _, a := range c.Args {
			if strings.HasPrefix(a, "--image-credential-provider") {
				t.Errorf("atelet keeps %s", a)
			}
		}
		for _, m := range c.VolumeMounts {
			if strings.HasPrefix(m.Name, "image-credential-provider") {
				t.Errorf("atelet keeps mount %s", m.Name)
			}
		}
	}
	for _, c := range pod.InitContainers {
		if c.Name != "objectstore-plugin" {
			continue
		}
		plugin = true
		for _, env := range c.Env {
			if env.Name == "AWS_ACCESS_KEY_ID" && (env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil) {
				t.Error("objectstore-plugin reads AWS_ACCESS_KEY_ID from a literal, want the ate-s3-credentials Secret")
			}
		}
	}
	if !atelet || !plugin {
		t.Errorf("atelet container present = %v, objectstore-plugin present = %v", atelet, plugin)
	}
}

// The bundle must render, and must not bring the bundled collector or
// prometheus that the fleet already runs.
func TestRenderK0sBundle(t *testing.T) {
	cfg := &config.Config{Root: repoRoot(t), Kind: true, ProfileDir: "k0s", Router: config.RouterEnvoy}
	e := &Env{Cfg: cfg}
	rendered, err := e.render(cfg.Path(SystemOverlay(cfg)))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, deployments := dataPlaneWorkloads(t, rendered)
	for _, name := range []string{"ate-api-server", "ate-controller", "rustfs"} {
		if _, ok := deployments[name]; !ok {
			t.Errorf("bundle has no Deployment %s", name)
		}
	}
	for _, name := range []string{"opentelemetry-collector", "prometheus"} {
		if _, ok := deployments[name]; ok {
			t.Errorf("bundle has Deployment %s, which the fleet provides", name)
		}
	}
}
