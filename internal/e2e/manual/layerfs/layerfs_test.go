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

// Package layerfs runs the LayerFS spike image as a micro-VM actor, on a pool
// with guestDevices: [fuse] and on a pool without it. It is not in
// internal/e2e/suites, so the normal e2e lanes do not run it. The workflow
// .github/workflows/layerfs-e2e.yaml runs it.
package layerfs

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

//go:embed run.py
var runPy string

// result holds the key=value pairs from one actor's RESULT and AX_PROBE
// lines, and the times the test measures itself.
type result struct {
	kv        map[string]string
	firstPass map[string]string // AX_PROBE warm=false
	warmPass  map[string]string // AX_PROBE warm=true
	resume    time.Duration
	run       time.Duration
	raw       string
}

func TestLayerFS(t *testing.T) {
	image := os.Getenv("LAYERFS_IMAGE")
	if image == "" || !e2e.IsMicroVM() {
		t.Fatalf("set LAYERFS_IMAGE (pinned by digest) and E2E_SANDBOX_CLASS=microvm; got %q and %q", image, e2e.SandboxClass())
	}
	env, err := e2e.CheckEnv("BUCKET_NAME", "KO_DOCKER_REPO")
	if err != nil {
		t.Fatalf("CheckEnv failed: %v", err)
	}
	script, err := json.Marshal(runPy)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	clients := e2e.GetClients()
	namespace, _ := e2e.DeploySubstrateFixture(t, ctx, clients, e2e.SubstrateFixtureManifests{
		Pool:     "internal/e2e/fixtures/guestdevices/guestdevices.yaml.tmpl",
		Template: "internal/e2e/manual/layerfs/layerfs-templates.yaml.tmpl",
		Vars:     map[string]string{"${LAYERFS_IMAGE}": image, "${LAYERFS_RUN_PY}": string(script)},
	}, env["BUCKET_NAME"], "layerfs", false)

	fuse := runActor(t, ctx, clients, namespace, "layerfs-fuse")
	nofuse := runActor(t, ctx, clients, namespace, "layerfs-nofuse")
	writeSummary(t, fuse, nofuse)

	if !strings.HasPrefix(fuse.kv["dev_fuse"], "character_special_file") {
		t.Errorf("fuse pool: dev_fuse = %q, want a character device", fuse.kv["dev_fuse"])
	}
	for _, k := range []string{"init", "mount_first", "mount_warm", "overlay_write"} {
		if fuse.kv[k] != "ok" {
			t.Errorf("fuse pool: %s = %q, want ok", k, fuse.kv[k])
		}
	}
	for _, p := range []map[string]string{fuse.firstPass, fuse.warmPass} {
		if p["command_ok"] != "true" || p["unmount_ok"] != "true" {
			t.Errorf("fuse pool: mount pass %v, want command_ok and unmount_ok true", p)
		}
	}
	if nofuse.kv["dev_fuse"] != "missing" {
		t.Errorf("pool without guestDevices: dev_fuse = %q, want missing", nofuse.kv["dev_fuse"])
	}
	if !strings.HasPrefix(nofuse.kv["mount_first"], "failed") {
		t.Errorf("pool without guestDevices: mount_first = %q, want failed", nofuse.kv["mount_first"])
	}
}

// runActor creates and resumes an actor from template, runs the LayerFS test
// in it and parses the output.
func runActor(t *testing.T, ctx context.Context, clients *e2e.Clients, namespace, template string) result {
	t.Helper()
	ref := &ateapipb.ObjectRef{Atespace: namespace, Name: template + "-actor"}
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: namespace, Name: ref.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: namespace, Name: template},
	}}); err != nil {
		t.Fatalf("CreateActor %q: %v", ref.Name, err)
	}
	t.Cleanup(func() {
		// DeleteActor requires the actor to be suspended.
		_, _ = clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: ref})
		_, _ = clients.SubstrateAPI.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref})
	})
	start := time.Now()
	if _, err := e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapipb.ResumeActorRequest{Actor: ref}); err != nil {
		t.Fatalf("ResumeActor %q: %v", ref.Name, err)
	}
	r := result{resume: time.Since(start)}

	rc, err := e2e.NewRouterClient(ctx)
	if err != nil {
		t.Fatalf("NewRouterClient: %v", err)
	}
	defer rc.Close()
	actor := resources.ActorRef{Atespace: namespace, Name: ref.Name}
	get := func(path string) (int, []byte) {
		resp, err := rc.Get(ctx, actor, path)
		if err != nil {
			t.Fatalf("GET %s for %q: %v", path, ref.Name, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}
	// The test runs longer than one request can wait, so run.py runs it in
	// the background and the test polls for the result.
	start = time.Now()
	if code, body := get("/run"); code != http.StatusOK {
		t.Fatalf("GET /run for %q: status %d, body %q", ref.Name, code, body)
	}
	var body []byte
	for {
		code, b := get("/result")
		body = b
		if code == http.StatusOK {
			break
		}
		if code != http.StatusAccepted {
			t.Fatalf("GET /result for %q: status %d, body %q", ref.Name, code, b)
		}
		if time.Since(start) > 15*time.Minute {
			t.Fatalf("GET /result for %q: not done after 15m; output so far:\n%s", ref.Name, b)
		}
		time.Sleep(2 * time.Second)
	}
	r.run = time.Since(start)
	r.raw = string(body)
	t.Logf("GET /run for %q:\n%s", ref.Name, body)
	r.kv, r.firstPass, r.warmPass = parse(r.raw)
	return r
}

// parse reads the key=value pairs from RESULT and AX_PROBE lines. A value
// runs to the next space, so run.py writes spaces in values as "_".
func parse(out string) (kv, first, warm map[string]string) {
	kv, first, warm = map[string]string{}, map[string]string{}, map[string]string{}
	for line := range strings.Lines(out) {
		tag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		into := kv
		switch {
		case tag == "AX_PROBE" && strings.Contains(rest, "warm=true"):
			into = warm
		case tag == "AX_PROBE" && strings.Contains(rest, "warm=false"):
			into = first
		case tag != "AX_PROBE" && tag != "RESULT":
			continue
		}
		for field := range strings.FieldsSeq(rest) {
			if k, v, ok := strings.Cut(field, "="); ok {
				into[k] = v
			}
		}
	}
	return kv, first, warm
}

// writeSummary appends a Markdown table to $GITHUB_STEP_SUMMARY, if it is set.
func writeSummary(t *testing.T, fuse, nofuse result) {
	t.Helper()
	or := func(v string) string {
		if v == "" {
			return "-"
		}
		return strings.ReplaceAll(v, "_", " ")
	}
	rows := [][3]string{
		{"/dev/fuse in the guest", or(fuse.kv["dev_fuse"]), or(nofuse.kv["dev_fuse"])},
		{"Sample tree (entries, bytes)", or(fuse.kv["tree_entries"]) + ", " + or(fuse.kv["tree_bytes"]), or(nofuse.kv["tree_entries"]) + ", " + or(nofuse.kv["tree_bytes"])},
		{"Actor resume (s)", fmt.Sprintf("%.2f", fuse.resume.Seconds()), fmt.Sprintf("%.2f", nofuse.resume.Seconds())},
		{"Init: seal the store (s)", or(fuse.kv["init_s"]), or(nofuse.kv["init_s"])},
		{"Store bytes", or(fuse.kv["store_bytes"]), or(nofuse.kv["store_bytes"])},
		{"First mount: result", or(fuse.kv["mount_first"]), or(nofuse.kv["mount_first"])},
		{"First mount: daemon start (s)", or(fuse.firstPass["daemon_start_s"]), or(nofuse.firstPass["daemon_start_s"])},
		{"First mount: install store (s)", or(fuse.firstPass["install_s"]), or(nofuse.firstPass["install_s"])},
		{"First mount: mount (s)", or(fuse.firstPass["mount_s"]), or(nofuse.firstPass["mount_s"])},
		{"First mount: read and write command (s)", or(fuse.firstPass["command_s"]), or(nofuse.firstPass["command_s"])},
		{"Warm mount: result", or(fuse.kv["mount_warm"]), or(nofuse.kv["mount_warm"])},
		{"Warm mount: daemon start (s)", or(fuse.warmPass["daemon_start_s"]), or(nofuse.warmPass["daemon_start_s"])},
		{"Warm mount: mount (s)", or(fuse.warmPass["mount_s"]), or(nofuse.warmPass["mount_s"])},
		{"Warm mount: read and write command (s)", or(fuse.warmPass["command_s"]), or(nofuse.warmPass["command_s"])},
		{"Files read in the mount", or(fuse.kv["read_files"]), or(nofuse.kv["read_files"])},
		{"Write in the overlay", or(fuse.kv["overlay_write"]), or(nofuse.kv["overlay_write"])},
		{"Whole test in the actor (s)", fmt.Sprintf("%.2f", fuse.run.Seconds()), fmt.Sprintf("%.2f", nofuse.run.Seconds())},
	}
	var b strings.Builder
	b.WriteString("## LayerFS on micro-VM actors\n\n| Step | Pool with `guestDevices: [fuse]` | Pool without it |\n|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r[0], r[1], r[2])
	}
	t.Logf("summary:\n%s", b.String())
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Errorf("open GITHUB_STEP_SUMMARY: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		t.Errorf("write GITHUB_STEP_SUMMARY: %v", err)
	}
}
