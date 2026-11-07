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

# Copy the gVisor release tarballs (amd64 and arm64) from the public gvisor
# bucket to an S3 location. Check each sha256 before the upload.
# Usage: GVISOR_MIRROR_S3_URI=s3://BUCKET/PREFIX hack/aws/mirror-runsc.sh
# Use AWS_PROFILE or the AWS credential chain for the upload.
# The release and the hashes must match manifests/ate-install/aws/sandboxconfig-gvisor.yaml.

set -o errexit -o nounset -o pipefail

: "${GVISOR_MIRROR_S3_URI:?Set GVISOR_MIRROR_S3_URI to s3://BUCKET/PREFIX}"
RELEASE="${GVISOR_RELEASE:-2026-09-02}"
BASE="https://storage.googleapis.com/gvisor/releases/nightly/${RELEASE}"
DEST="${GVISOR_MIRROR_S3_URI%/}/${RELEASE}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# "ARCH SHA256" pairs for the default release.
while read -r arch sum; do
  file="${work}/${arch}.tar.zstd"
  curl --fail --silent --show-error --location "${BASE}/${arch}/gvisor.tar.zstd" --output "${file}"
  echo "${sum}  ${file}" | shasum -a 256 --check --status
  aws s3 cp "${file}" "${DEST}/${arch}/gvisor.tar.zstd"
done <<'EOF'
x86_64 d547d81401461fd1c679c5c4fa0a6c2b8ef7dc3c22ce23c9e25dcc4c69cfd06f
aarch64 a64916f9813ce7e4841a30480a599337f7dda07b421c6bf0123db2212aa7d1df
EOF
