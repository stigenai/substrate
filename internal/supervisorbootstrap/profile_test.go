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

package supervisorbootstrap_test

import (
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap"
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap/supervisorbootstraptest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"testing"
)

func TestDescribe(t *testing.T) {
	template := supervisorbootstraptest.Template()
	got, err := supervisorbootstrap.Describe(template)
	if err != nil || got == nil {
		t.Fatalf("Describe: %v", err)
	}
	template.Metadata.Version = 42
	again, err := supervisorbootstrap.Describe(template)
	if err != nil || again.TemplateDigest != got.TemplateDigest {
		t.Fatal("metadata changed template digest")
	}
	template.Containers[0].Env[0].Value = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	changed, err := supervisorbootstrap.Describe(template)
	if err != nil || changed.TemplateDigest == got.TemplateDigest {
		t.Fatal("launch binding not hashed")
	}
	if ordinary, err := supervisorbootstrap.Describe(&ateapipb.ActorTemplate{}); ordinary != nil || err != nil {
		t.Fatal("ordinary template affected")
	}
}

func TestDescribeDenials(t *testing.T) {
	for name, mutate := range map[string]func(*ateapipb.ActorTemplate){
		"profile": func(v *ateapipb.ActorTemplate) {
			v.Volumes[0].GetSystemInfo().DataSources[0].GetSupervisorBootstrap().Profile = "other"
		},
		"memory": func(v *ateapipb.ActorTemplate) {
			v.SnapshotConfig.OnPause = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
		},
		"image":   func(v *ateapipb.ActorTemplate) { v.Containers[0].Image = "worker:latest" },
		"command": func(v *ateapipb.ActorTemplate) { v.Containers[0].Command = []string{"sh"} },
		"mount":   func(v *ateapipb.ActorTemplate) { v.Containers[0].VolumeMounts[0].MountPath = "/tmp" },
		"launch":  func(v *ateapipb.ActorTemplate) { v.Containers[0].Env = nil },
		"sidecar": func(v *ateapipb.ActorTemplate) { v.Containers = append(v.Containers, v.Containers[0]) },
		"mixed": func(v *ateapipb.ActorTemplate) {
			v.Volumes[0].GetSystemInfo().DataSources = append(v.Volumes[0].GetSystemInfo().DataSources, &ateapipb.SystemInfoDataSource{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := supervisorbootstraptest.Template()
			mutate(v)
			if _, err := supervisorbootstrap.Describe(v); err == nil {
				t.Fatal("accepted unsafe template")
			}
		})
	}
}
