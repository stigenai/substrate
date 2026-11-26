#!/usr/bin/env bash

# Copyright 2026 The Agent Substrate Authors
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

# Copy the gVisor release tarballs that manifests/ate-install/sandboxconfig-gvisor.yaml
# names (each release, amd64 and arm64) from the public gvisor bucket to an S3
# location. Check each sha256 before the upload.
# Usage: GVISOR_MIRROR_S3_URI=s3://BUCKET/PREFIX hack/aws/mirror-runsc.sh
# Use AWS_PROFILE or the default AWS provider chain for the upload.
# The upload paths match manifests/ate-install/aws/sandboxconfig-gvisor.yaml.

set -o errexit -o nounset -o pipefail

: "${GVISOR_MIRROR_S3_URI:?Set GVISOR_MIRROR_S3_URI to s3://BUCKET/PREFIX}"

ROOT="$(git rev-parse --show-toplevel)"
SRC_URL_PREFIX="gs://gvisor/releases/release/"
DEST="${GVISOR_MIRROR_S3_URI%/}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Each "url:" line is followed by its "sha256:" line.
awk '/url: "gs:\/\//{gsub(/"/,"",$2); url=$2} /sha256:/{gsub(/"/,"",$2); print url, $2}' \
  "${ROOT}/manifests/ate-install/sandboxconfig-gvisor.yaml" |
  while read -r url sum; do
    path="${url#"${SRC_URL_PREFIX}"}" # RELEASE/ARCH/gvisor.tar.zstd
    file="${work}/$(echo "${path}" | tr / _)"
    curl --fail --silent --show-error --location \
      "https://storage.googleapis.com/gvisor/releases/release/${path}" --output "${file}"
    echo "${sum}  ${file}" | shasum -a 256 --check --status
    aws s3 cp "${file}" "${DEST}/${path}"
  done
