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

// Package supervisorbootstraptest supplies an opt-in template for boundary tests.
package supervisorbootstraptest

import (
	"strings"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/supervisorbootstrap"
)

func Template() *ateapi.ActorTemplate {
	return &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: "team-a", Name: "template-1", Uid: "11111111-1111-4111-8111-111111111111"},
		Containers:     []*ateapi.Container{{Name: "main", Image: "example/worker@sha256:" + strings.Repeat("a", 64), Command: []string{"/usr/local/bin/ax-task-runner"}, Env: []*ateapi.EnvVar{{Name: "FACTORY_NATIVE_LAUNCH_DIGEST", Value: "sha256:" + strings.Repeat("b", 64)}}, VolumeMounts: []*ateapi.VolumeMount{{Name: "bootstrap", MountPath: supervisorbootstrap.MountPath}}}},
		Volumes:        []*ateapi.Volume{{Name: "bootstrap", SystemInfo: &ateapi.SystemInfoVolumeSource{DataSources: []*ateapi.SystemInfoDataSource{{SupervisorBootstrap: &ateapi.SupervisorBootstrapDataSource{Profile: supervisorbootstrap.Profile}}}}}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS},
	}
}
