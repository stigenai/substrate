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

package config

import "testing"

func TestProfile(t *testing.T) {
	if got := (&Config{}).Profile(); got != "kind" {
		t.Errorf("Profile() = %q, want kind", got)
	}
	loadEnv(t)
	t.Setenv("ATE_INSTALL_KIND", "true")
	t.Setenv("ATE_INSTALL_PROFILE", "k0s")
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Profile(); got != "k0s" {
		t.Errorf("Profile() = %q, want k0s", got)
	}
}
