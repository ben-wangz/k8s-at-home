#! /bin/bash

set -o errexit
set -o nounset
set -o pipefail

readonly CT_VERSION=v3.13.0
readonly KIND_VERSION=v0.29.0
readonly K8S_VERSION=v1.33.1
readonly CLUSTER_NAME=chart-testing

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
if ! command -v podman &> /dev/null
then
  echo "podman could not be found"
  exit 1
fi

podman run --rm \
  -v "${SCRIPT_DIR}/..:/workspace/code" \
  -w /workspace/code \
  quay.io/helmpack/chart-testing:${CT_VERSION} ct lint \
    --chart-dirs application/aria2 \
    --chart-dirs application/clash \
    --chart-dirs application/yacd
