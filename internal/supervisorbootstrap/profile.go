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

// Package supervisorbootstrap defines the narrow opt-in Fabriek projection.
package supervisorbootstrap

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/proto"
)

const (
	Profile   = "fabriek-native-v1"
	Audience  = "urn:fabriek:native-bootstrap"
	Purpose   = "fabriek-supervisor-bootstrap"
	Schema    = "substrate.supervisor-bootstrap/v1"
	MountPath = "/run/substrate/fabriek-supervisor"
	FileName  = "activation.jwt"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Describe validates the whole opt-in boundary. Nil means an ordinary template.
func Describe(t *ateapipb.ActorTemplate) (*ateapipb.SupervisorActivation, error) {
	selected, err := projection(t)
	if err != nil || selected == nil {
		return nil, err
	}
	if len(t.GetContainers()) != 1 {
		return nil, fmt.Errorf("supervisor bootstrap requires one container")
	}
	c := t.GetContainers()[0]
	if err := validateContainer(c, selected.GetName()); err != nil {
		return nil, err
	}
	config := t.GetSnapshotConfig()
	if config.GetOnPause() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA || config.GetOnCommit() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA || config.GetOnResume().GetFromData() != ateapipb.ResumeSource_RESUME_SOURCE_GOLDEN {
		return nil, fmt.Errorf("supervisor bootstrap requires Data snapshots and Golden resume")
	}
	launch, image, err := binding(c)
	if err != nil {
		return nil, err
	}
	canonical := proto.CloneOf(t)
	canonical.Metadata, canonical.Status = nil, nil
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(canonical)
	if err != nil {
		return nil, err
	}
	return &ateapipb.SupervisorActivation{Profile: Profile, TemplateUid: t.GetMetadata().GetUid(), TemplateDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(raw)), ImageDigest: image, LaunchDigest: launch}, nil
}

func projection(t *ateapipb.ActorTemplate) (*ateapipb.Volume, error) {
	var selected *ateapipb.Volume
	for _, v := range t.GetVolumes() {
		for _, ds := range v.GetSystemInfo().GetDataSources() {
			if b := ds.GetSupervisorBootstrap(); b != nil {
				if selected != nil || b.GetProfile() != Profile || len(v.GetSystemInfo().GetDataSources()) != 1 {
					return nil, fmt.Errorf("supervisor bootstrap requires one dedicated %s projection", Profile)
				}
				selected = v
			}
		}
	}
	return selected, nil
}

func validateContainer(c *ateapipb.Container, volume string) error {
	if len(c.GetCommand()) != 1 || c.GetCommand()[0] != "/usr/local/bin/ax-task-runner" || len(c.GetArgs()) != 0 {
		return fmt.Errorf("supervisor bootstrap requires the fixed AX runner")
	}
	found := false
	for _, m := range c.GetVolumeMounts() {
		if m.GetName() == volume {
			if m.GetMountPath() != MountPath || found {
				return fmt.Errorf("supervisor bootstrap mount path differs")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("supervisor bootstrap volume is not mounted")
	}
	return nil
}

func binding(c *ateapipb.Container) (string, string, error) {
	launch := ""
	for _, e := range c.GetEnv() {
		if e.GetName() == "FACTORY_NATIVE_LAUNCH_DIGEST" {
			if launch != "" || !digestPattern.MatchString(e.GetValue()) {
				return "", "", fmt.Errorf("supervisor launch digest invalid")
			}
			launch = e.GetValue()
		}
	}
	_, image, ok := strings.Cut(c.GetImage(), "@")
	if launch == "" || !ok || !digestPattern.MatchString(image) {
		return "", "", fmt.Errorf("supervisor image and launch digests required")
	}
	return launch, image, nil
}
