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

// Package supervisorbootstraptest supplies an opt-in template for boundary tests.
package supervisorbootstraptest

import (
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"strings"
)

func Template() *ateapipb.ActorTemplate {
	return &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "template-1", Uid: "11111111-1111-4111-8111-111111111111"},
		Containers:     []*ateapipb.Container{{Name: "main", Image: "example/worker@sha256:" + strings.Repeat("a", 64), Command: []string{"/usr/local/bin/ax-task-runner"}, Env: []*ateapipb.EnvVar{{Name: "FACTORY_NATIVE_LAUNCH_DIGEST", Value: "sha256:" + strings.Repeat("b", 64)}}, VolumeMounts: []*ateapipb.VolumeMount{{Name: "bootstrap", MountPath: supervisorbootstrap.MountPath}}}},
		Volumes:        []*ateapipb.Volume{{Name: "bootstrap", SystemInfo: &ateapipb.SystemInfoVolumeSource{DataSources: []*ateapipb.SystemInfoDataSource{{SupervisorBootstrap: &ateapipb.SupervisorBootstrapDataSource{Profile: supervisorbootstrap.Profile}}}}}},
		SnapshotConfig: &ateapipb.SnapshotConfig{OnPause: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, OnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, OnResume: &ateapipb.OnResumeConfig{FromData: ateapipb.ResumeSource_RESUME_SOURCE_GOLDEN}},
	}
}
