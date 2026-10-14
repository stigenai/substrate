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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/supervisorbootstrap"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type fakeSupervisorReader struct {
	actor, last *ateapipb.Actor
	template    *ateapipb.ActorTemplate
	calls       []string
	errAt       int
}

func (f *fakeSupervisorReader) GetActor(_ context.Context, request *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.calls = append(f.calls, "actor:"+request.GetActor().GetAtespace()+"/"+request.GetActor().GetName())
	if len(f.calls) == f.errAt {
		return nil, errors.New("secret remote diagnostic")
	}
	if len(f.calls) > 1 {
		return proto.CloneOf(f.last), nil
	}
	return proto.CloneOf(f.actor), nil
}

func (f *fakeSupervisorReader) GetActorTemplate(_ context.Context, request *ateapipb.GetActorTemplateRequest, _ ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	f.calls = append(f.calls, "template:"+request.GetActorTemplate().GetAtespace()+"/"+request.GetActorTemplate().GetName())
	if len(f.calls) == f.errAt {
		return nil, errors.New("secret remote diagnostic")
	}
	return proto.CloneOf(f.template), nil
}

func supervisorFixture(t *testing.T) *fakeSupervisorReader {
	t.Helper()
	template := &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "factory", Name: "template", Uid: "template-uid", Version: 1},
		Containers: []*ateapipb.Container{{
			Name: "guest", Image: "worker@sha256:" + strings.Repeat("a", 64), Command: []string{"/usr/local/bin/ax-task-runner"},
			Env:          []*ateapipb.EnvVar{{Name: "FACTORY_NATIVE_LAUNCH_DIGEST", Value: "sha256:" + strings.Repeat("b", 64)}, {Name: "SECRET_MARKER", Value: "never-print-this"}},
			VolumeMounts: []*ateapipb.VolumeMount{{Name: "protected", MountPath: supervisorbootstrap.MountPath}},
			Resources:    &ateapipb.Resources{Limits: []*ateapipb.Limits{{Name: "cpu", Quantity: "0.5"}, {Name: "memory", Quantity: "536870912"}}},
		}},
		Volumes:        []*ateapipb.Volume{{Name: "protected", SystemInfo: &ateapipb.SystemInfoVolumeSource{DataSources: []*ateapipb.SystemInfoDataSource{{SupervisorBootstrap: &ateapipb.SupervisorBootstrapDataSource{Profile: supervisorbootstrap.Profile}}}}}},
		SnapshotConfig: &ateapipb.SnapshotConfig{OnPause: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, OnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, OnResume: &ateapipb.OnResumeConfig{FromData: ateapipb.ResumeSource_RESUME_SOURCE_GOLDEN}},
	}
	activation, err := supervisorbootstrap.Describe(template)
	if err != nil {
		t.Fatal(err)
	}
	activation.ActivationId, activation.WorkerUid = "activation-uid", "worker-uid"
	actor := &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: "factory", Name: "actor", Uid: "actor-uid", Version: 7},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "factory", Name: "template"},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING, SupervisorActivation: activation,
			WorkerAssignment: &ateapipb.WorkerAssignment{WorkerPodUid: "worker-uid", NodeName: "worker-node"}},
	}
	return &fakeSupervisorReader{actor: actor, last: proto.CloneOf(actor), template: template}
}

func observeSupervisor(f *fakeSupervisorReader) (string, error) {
	var out bytes.Buffer
	err := writeSupervisorObservation(context.Background(), f, &ateapipb.ObjectRef{Atespace: "factory", Name: "actor"}, &out)
	return out.String(), err
}

func TestSupervisorObservation(t *testing.T) {
	f := supervisorFixture(t)
	out, err := observeSupervisor(f)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls, []string{"actor:factory/actor", "template:factory/template", "actor:factory/actor"}) {
		t.Fatalf("read sequence: %v", f.calls)
	}
	if strings.Contains(out, "SECRET_MARKER") || strings.Contains(out, "never-print-this") || strings.Contains(out, "activation.jwt") {
		t.Fatalf("private template data escaped: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"schema_version": "substrate.supervisor-observation/v1", "atespace": "factory", "actor_name": "actor", "actor_uid": "actor-uid", "state": "ACTOR_STATE_RUNNING",
		"assignment": map[string]any{"worker_uid": "worker-uid", "node_name": "worker-node"},
		"activation": map[string]any{"activation_id": "activation-uid", "worker_uid": "worker-uid", "template_uid": "template-uid", "template_digest": f.actor.Status.SupervisorActivation.TemplateDigest, "image_digest": "sha256:" + strings.Repeat("a", 64), "launch_digest": "sha256:" + strings.Repeat("b", 64), "profile": supervisorbootstrap.Profile},
		"template":   map[string]any{"atespace": "factory", "name": "template", "uid": "template-uid", "digest": f.actor.Status.SupervisorActivation.TemplateDigest, "resource_limits": map[string]any{"cpu": "500m", "memory": "536870912"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation mismatch:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestSupervisorObservationRejectsChangesAndInvalidBindings(t *testing.T) {
	for name, mutate := range map[string]func(*fakeSupervisorReader){
		"actor changed":              func(f *fakeSupervisorReader) { f.last.Metadata.Uid = "recreated" },
		"version changed":            func(f *fakeSupervisorReader) { f.last.Metadata.Version++ },
		"state changed":              func(f *fakeSupervisorReader) { f.last.Status.State = ateapipb.ActorState_ACTOR_STATE_PAUSING },
		"activation changed":         func(f *fakeSupervisorReader) { f.last.Status.SupervisorActivation.ActivationId = "next" },
		"assignment changed":         func(f *fakeSupervisorReader) { f.last.Status.WorkerAssignment.NodeName = "next" },
		"template reference changed": func(f *fakeSupervisorReader) { f.last.ActorTemplate.Name = "next" },
		"wrong actor":                func(f *fakeSupervisorReader) { f.actor.Metadata.Name = "another" },
		"missing actor":              func(f *fakeSupervisorReader) { f.actor = nil },
		"unversioned actor":          func(f *fakeSupervisorReader) { f.actor.Metadata.Version = 0 },
		"wrong template":             func(f *fakeSupervisorReader) { f.template.Metadata.Name = "another" },
		"template recreated":         func(f *fakeSupervisorReader) { f.template.Metadata.Uid = "another" },
		"missing template":           func(f *fakeSupervisorReader) { f.template = nil },
		"changed template":           func(f *fakeSupervisorReader) { f.template.Containers[0].Env[1].Value = "changed" },
		"ordinary template":          func(f *fakeSupervisorReader) { f.template.Volumes = nil },
		"unsafe projection":          func(f *fakeSupervisorReader) { f.template.Containers[0].VolumeMounts[0].MountPath = "/tmp" },
		"unsafe snapshot": func(f *fakeSupervisorReader) {
			f.template.SnapshotConfig.OnPause = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
		},
		"missing activation":  func(f *fakeSupervisorReader) { f.actor.Status.SupervisorActivation = nil },
		"wrong worker":        func(f *fakeSupervisorReader) { f.actor.Status.WorkerAssignment.WorkerPodUid = "another" },
		"missing assignment":  func(f *fakeSupervisorReader) { f.actor.Status.WorkerAssignment = nil },
		"missing node":        func(f *fakeSupervisorReader) { f.actor.Status.WorkerAssignment.NodeName = "" },
		"first read error":    func(f *fakeSupervisorReader) { f.errAt = 1 },
		"template read error": func(f *fakeSupervisorReader) { f.errAt = 2 },
		"second read error":   func(f *fakeSupervisorReader) { f.errAt = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			f := supervisorFixture(t)
			mutate(f)
			out, err := observeSupervisor(f)
			if err == nil || out != "" || strings.Contains(err.Error(), "secret") || len(err.Error()) > 128 {
				t.Fatalf("expected bounded denial without output: %q, %v", out, err)
			}
		})
	}
}

func TestSupervisorLimits(t *testing.T) {
	for _, limits := range [][]*ateapipb.Limits{
		nil,
		{{Name: "cpu", Quantity: "0.5"}},
		{{Name: "cpu", Quantity: "0"}, {Name: "memory", Quantity: "512Mi"}},
		{{Name: "cpu", Quantity: "-1"}, {Name: "memory", Quantity: "512Mi"}},
		{{Name: "cpu", Quantity: "bad"}, {Name: "memory", Quantity: "512Mi"}},
		{{Name: "cpu", Quantity: "1"}, {Name: "cpu", Quantity: "2"}, {Name: "memory", Quantity: "512Mi"}},
	} {
		if _, err := supervisorLimits(&ateapipb.Resources{Limits: limits}); err == nil {
			t.Fatalf("accepted limits: %v", limits)
		}
	}
	got, err := supervisorLimits(&ateapipb.Resources{Limits: []*ateapipb.Limits{{Name: "cpu", Quantity: "0.5"}, {Name: "memory", Quantity: "0.5Gi"}}})
	if err != nil || got["cpu"] != "500m" || got["memory"] != "512Mi" {
		t.Fatalf("canonical limits: %v, %v", got, err)
	}
}

func TestSupervisorCommandArgs(t *testing.T) {
	runCommandArgsTests(t, []commandArgsTest{
		{name: "missing", command: getSupervisorActivationCmd, wantErr: true},
		{name: "one", command: getSupervisorActivationCmd, args: []string{"actor"}},
		{name: "many", command: getSupervisorActivationCmd, args: []string{"actor", "other"}, wantErr: true},
	})
}
