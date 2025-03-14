#!/usr/bin/env bash

# Copyright 2020 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail
set -o xtrace

echo "CLOUD_PROVIDER=${CLOUD_PROVIDER}"
echo "CLUSTER_NAME=${CLUSTER_NAME}"

if [[ -n "${KOPS_BASE_URL-}" ]]; then
    unset KOPS_BASE_URL
fi

if [[ -z "${WORKSPACE-}" ]]; then
    export WORKSPACE
    WORKSPACE=$(mktemp -dt kops.XXXXXXXXX)
fi

export KOPS_FEATURE_FLAGS="SpecOverrideFlag,${KOPS_FEATURE_FLAGS:-}"
export GO111MODULE=on

if [[ -z "${AWS_SSH_PRIVATE_KEY_FILE-}" ]]; then
    export AWS_SSH_PRIVATE_KEY_FILE="${HOME}/.ssh/id_rsa"
fi
if [[ -z "${AWS_SSH_PUBLIC_KEY_FILE-}" ]]; then
    export AWS_SSH_PUBLIC_KEY_FILE="${HOME}/.ssh/id_rsa.pub"
fi

KUBETEST2="kubetest2 kops -v=2 --cloud-provider=${CLOUD_PROVIDER} --cluster-name=${CLUSTER_NAME:-}"
KUBETEST2="${KUBETEST2} --admin-access=${ADMIN_ACCESS:-}"

# Always tear-down the cluster when we're done
function kops-finish {
  # shellcheck disable=SC2153
  ${KUBETEST2} --kops-binary-path="${KOPS}" --down || echo "kubetest2 down failed"
}
trap kops-finish EXIT

make test-e2e-install

function kops-download-release() {
    local kops
    kops=$(mktemp -t kops.XXXXXXXXX)
    wget -qO "${kops}" "https://github.com/kubernetes/kops/releases/download/${1}/kops-$(go env GOOS)-$(go env GOARCH)"
    chmod +x "${kops}"
    echo "${kops}"
}

function kops-download-from-base() {
    local kops
    kops=$(mktemp -t kops.XXXXXXXXX)
    wget -qO "${kops}" "$KOPS_BASE_URL/$(go env GOOS)/$(go env GOARCH)/kops"
    chmod +x "${kops}"
    echo "${kops}"
}

function kops-base-from-marker() {
    if [[ "${1}" == "latest" ]]; then
        curl -s "https://storage.googleapis.com/kops-ci/bin/latest-ci-updown-green.txt"
    else
        curl -s "https://storage.googleapis.com/k8s-staging-kops/kops/releases/markers/release-${1}/latest-ci.txt"
    fi
}