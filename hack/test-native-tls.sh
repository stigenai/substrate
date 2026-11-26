#!/usr/bin/env bash

# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Runs the native TLS integration tests against the real Envoy dataplane image.
# Needs Docker and Go. Fails if the Envoy binary is missing.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

test_dir="$(mktemp -d)"
trap 'rm -rf "${test_dir}"' EXIT

docker build --platform "linux/${arch}" --iidfile "${test_dir}/image" \
  -f cmd/dataplane/envoy/Dockerfile cmd/dataplane/envoy
GOOS=linux GOARCH="${arch}" CGO_ENABLED=0 go test -c -tags=native_tls_integration \
  -o "${test_dir}/native-tls.test" ./cmd/atenet/internal/router/egress
docker run --rm --platform "linux/${arch}" --network none --cap-drop ALL --read-only --tmpfs /tmp \
  --add-host api.test:127.0.0.1 --add-host chat.test:127.0.0.1 \
  -e SUBSTRATE_NATIVE_TLS_ENVOY=/usr/local/bin/envoy \
  -e SUBSTRATE_EGRESS_MANIFEST=/atenet-egress.yaml \
  -v "${ROOT}/manifests/ate-install/atenet-egress.yaml:/atenet-egress.yaml:ro" \
  -v "${test_dir}/native-tls.test:/native-tls.test:ro" \
  --entrypoint /native-tls.test "$(cat "${test_dir}/image")" \
  -test.run '^(TestNativeTLSEnvoy|TestEgressUpstreamSessionPerSNI)$' -test.v -test.timeout 120s
