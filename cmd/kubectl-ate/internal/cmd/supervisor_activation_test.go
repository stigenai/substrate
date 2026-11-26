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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/supervisorbootstrap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type fakeSupervisorReader struct {
	actor, last *ateapi.Actor
	template    *ateapi.ActorTemplate
	calls       []string
	errAt       int
}

func (f *fakeSupervisorReader) GetActor(_ context.Context, request *ateapi.GetActorRequest, _ ...grpc.CallOption) (*ateapi.Actor, error) {
	f.calls = append(f.calls, "actor:"+request.GetActor().GetAtespace()+"/"+request.GetActor().GetName())
	if len(f.calls) == f.errAt {
		return nil, errors.New("secret remote diagnostic")
	}
	if len(f.calls) > 1 {
		return proto.CloneOf(f.last), nil
	}
	return proto.CloneOf(f.actor), nil
}

func (f *fakeSupervisorReader) GetActorTemplate(_ context.Context, request *ateapi.GetActorTemplateRequest, _ ...grpc.CallOption) (*ateapi.ActorTemplate, error) {
	f.calls = append(f.calls, "template:"+request.GetActorTemplate().GetAtespace()+"/"+request.GetActorTemplate().GetName())
	if len(f.calls) == f.errAt {
		return nil, errors.New("secret remote diagnostic")
	}
	return proto.CloneOf(f.template), nil
}

func supervisorFixture(t *testing.T) *fakeSupervisorReader {
	t.Helper()
	template := &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: "factory", Name: "template", Uid: "template-uid", Version: 1},
		Containers: []*ateapi.Container{{
			Name: "guest", Image: "worker@sha256:" + strings.Repeat("a", 64), Command: []string{"/usr/local/bin/ax-task-runner"},
			Env:          []*ateapi.EnvVar{{Name: "FACTORY_NATIVE_LAUNCH_DIGEST", Value: "sha256:" + strings.Repeat("b", 64)}, {Name: "SECRET_MARKER", Value: "never-print-this"}},
			VolumeMounts: []*ateapi.VolumeMount{{Name: "protected", MountPath: supervisorbootstrap.MountPath}},
			Resources:    &ateapi.ResourceRequirements{Limits: &ateapi.Resources{CpuMillicores: 500, MemoryBytes: 512 << 20}},
		}},
		Volumes:        []*ateapi.Volume{{Name: "protected", SystemInfo: &ateapi.SystemInfoVolumeSource{DataSources: []*ateapi.SystemInfoDataSource{{SupervisorBootstrap: &ateapi.SupervisorBootstrapDataSource{Profile: supervisorbootstrap.Profile}}}}}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS},
	}
	activation, err := supervisorbootstrap.Describe(template)
	if err != nil {
		t.Fatal(err)
	}
	activation.ActivationId, activation.WorkerUid = "activation-uid", "worker-uid"
	actor := &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: "factory", Name: "actor", Uid: "actor-uid", Version: 7},
		ActorTemplate: &ateapi.ObjectRef{Atespace: "factory", Name: "template"},
		Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_RUNNING, SupervisorActivation: activation,
			WorkerAssignment: &ateapi.WorkerAssignment{WorkerPodUid: "worker-uid", NodeName: "worker-node"}},
	}
	return &fakeSupervisorReader{actor: actor, last: proto.CloneOf(actor), template: template}
}

func observeSupervisor(f *fakeSupervisorReader) (string, error) {
	var out bytes.Buffer
	err := writeSupervisorObservation(context.Background(), f, &ateapi.ObjectRef{Atespace: "factory", Name: "actor"}, &out)
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
		"template":   map[string]any{"atespace": "factory", "name": "template", "uid": "template-uid", "digest": f.actor.Status.SupervisorActivation.TemplateDigest, "resource_limits": map[string]any{"cpu": "500m", "memory": "512Mi"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation mismatch:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestSupervisorObservationRejectsChangesAndInvalidBindings(t *testing.T) {
	for name, mutate := range map[string]func(*fakeSupervisorReader){
		"actor changed":              func(f *fakeSupervisorReader) { f.last.Metadata.Uid = "recreated" },
		"version changed":            func(f *fakeSupervisorReader) { f.last.Metadata.Version++ },
		"state changed":              func(f *fakeSupervisorReader) { f.last.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDING },
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
			f.template.SnapshotConfig.PreferredFidelity = ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
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
	for name, limits := range map[string]*ateapi.Resources{
		"none":        nil,
		"no memory":   {CpuMillicores: 500},
		"no cpu":      {MemoryBytes: 512 << 20},
		"negative":    {CpuMillicores: -1, MemoryBytes: 512 << 20},
		"zero memory": {CpuMillicores: 1000, MemoryBytes: 0},
	} {
		if _, err := supervisorLimits(&ateapi.ResourceRequirements{Limits: limits}); err == nil {
			t.Fatalf("%s: accepted limits", name)
		}
	}
	got, err := supervisorLimits(&ateapi.ResourceRequirements{Limits: &ateapi.Resources{CpuMillicores: 2000, MemoryBytes: 512 << 20}})
	if err != nil || got["cpu"] != "2" || got["memory"] != "512Mi" {
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
