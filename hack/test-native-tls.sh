#!/usr/bin/env bash

# Copyright 2026 Google LLC
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

set -euo pipefail

# Run the actual Envoy/dataplane and two TLS legs in one disposable network
# namespace. No cluster, provider, or privileged runtime is involved.
cd "$(dirname "$0")/.."
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT

docker build --platform linux/amd64 --iidfile "$test_dir/image" \
	-f cmd/dataplane/envoy/Dockerfile cmd/dataplane/envoy
GOMAXPROCS=2 GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -p=2 -c -tags=native_tls_integration \
	-o "$test_dir/native-tls.test" ./cmd/atenet/internal/router/egress
GOMAXPROCS=2 GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -p=2 -c -tags=native_tls_integration \
	-o "$test_dir/router.test" ./cmd/atenet/internal/router
docker run --rm --network none --cap-drop ALL --read-only --tmpfs /tmp \
	--add-host api.test:127.0.0.1 --add-host chat.test:127.0.0.1 \
	-e SUBSTRATE_NATIVE_TLS_ENVOY=/usr/local/bin/envoy \
	-e SUBSTRATE_EGRESS_MANIFEST=/atenet-egress-with-sdsmint.yaml \
	-v "$PWD/manifests/ate-install/atenet-egress-with-sdsmint.yaml:/atenet-egress-with-sdsmint.yaml:ro" \
	-e SUBSTRATE_EGRESS_MANIFESTS=/manifests \
	-v "$PWD/manifests/ate-install:/manifests:ro" \
	-v "$test_dir/native-tls.test:/native-tls.test:ro" \
	-v "$test_dir/router.test:/router.test:ro" \
	--entrypoint /bin/sh "$(cat "$test_dir/image")" -ec '
/native-tls.test -test.run "^(TestNativeTLSEnvoy|TestEgressUpstreamSessionPerSNI|TestEgressConnectAuthorityPerTunnel)\$" -test.v -test.timeout 60s
/router.test -test.run "^TestIngressConnectTunnelsKeepTheirOwnTarget\$" -test.v -test.timeout 60s'
