#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Render the aws overlay, fill in sample placeholder values, and check it with
# kubeconform against the Kubernetes schemas and the Substrate CRDs.
# Fail if a gs:// URL or a GKE path remains in the output.
# Needs kustomize, kubeconform, envsubst and python3 with PyYAML.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# One JSON schema per CRD version, named <Kind>_<version>.json.
python3 -I - "${ROOT}/manifests/ate-install/generated" "${work}" <<'EOF'
import json, pathlib, sys, yaml

src, dst = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
for f in src.glob("ate.dev_*.yaml"):
    for doc in yaml.safe_load_all(f.read_text()):
        if not doc or doc.get("kind") != "CustomResourceDefinition":
            continue
        kind = doc["spec"]["names"]["kind"]
        for v in doc["spec"]["versions"]:
            (dst / f"{kind}_{v['name']}.json").write_text(
                json.dumps(v["schema"]["openAPIV3Schema"]))
EOF

kustomize build --load-restrictor LoadRestrictionsNone manifests/ate-install/aws |
  AWS_REGION=us-east-1 \
    ATE_API_ROLE_ARN=arn:aws:iam::111122223333:role/ate-api \
    ATE_ATELET_ROLE_ARN=arn:aws:iam::111122223333:role/ate-atelet \
    GVISOR_MIRROR_S3_URI=s3://example-bucket/gvisor \
    SUBSTRATE_VERSION=sample SUBSTRATE_VERSION_SUFFIX=sample \
    envsubst >"${work}/rendered.yaml"

if grep -nE 'gs://|/home/kubernetes|ko://' "${work}/rendered.yaml"; then
  echo "FAIL: the rendered output has a GKE reference or an unpinned ko:// image" >&2
  exit 1
fi

kubeconform -strict -summary \
  -schema-location default \
  -schema-location "${work}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json" \
  "${work}/rendered.yaml"
