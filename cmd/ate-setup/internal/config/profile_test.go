// Copyright 2026 Google LLC
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

func TestProfileDefaultsToKind(t *testing.T) {
	if got := (&Config{}).Profile(); got != "kind" {
		t.Errorf("Profile() = %q, want kind", got)
	}
	if got := (&Config{ProfileDir: "k0s"}).Profile(); got != "k0s" {
		t.Errorf("Profile() = %q, want k0s", got)
	}
}
