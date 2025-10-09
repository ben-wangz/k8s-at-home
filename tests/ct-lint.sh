#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

# Color output
readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly NC='\033[0m' # No Color

# Script directory
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
REPO_ROOT="${SCRIPT_DIR}/.."

# Default versions
readonly CT_VERSION=v3.13.0
readonly KIND_VERSION=v0.29.0
readonly K8S_VERSION=v1.33.1
readonly CLUSTER_NAME=chart-testing

# Check if podman is available
if ! command -v podman &> /dev/null; then
  echo -e "${RED}Error: podman could not be found${NC}"
  echo "Please install podman first"
  exit 1
fi

# Define charts to lint (matching the GitHub Actions workflow)
declare -A CHARTS=(
  ["aria2"]="application/aria2"
  ["clash"]="application/clash"
  ["yacd"]="application/yacd"
)

# Parse command line arguments
LINT_ALL=true
CHARTS_TO_LINT=()
CT_IMAGE=${CT_IMAGE:-"quay.io/helmpack/chart-testing:${CT_VERSION}"}

usage() {
  cat << EOF
Usage: $(basename "$0") [OPTIONS] [CHARTS...]

Lint Helm charts using chart-testing in a podman container.

OPTIONS:
  -h, --help          Show this help message

CHARTS:
  If no charts specified, all charts will be linted.
  Available charts: ${!CHARTS[@]}

ENVIRONMENT VARIABLES:
  CT_IMAGE              - Chart testing image (default: quay.io/helmpack/chart-testing:${CT_VERSION})

EXAMPLES:
  # Lint all charts
  $(basename "$0")

  # Lint specific charts
  $(basename "$0") aria2 clash

  # Lint with custom chart-testing image
  CT_IMAGE=m.daocloud.io/quay.io/helmpack/chart-testing:${CT_VERSION} $(basename "$0")

EOF
}

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo -e "${RED}Error: Unknown option $1${NC}"
      usage
      exit 1
      ;;
    *)
      LINT_ALL=false
      CHARTS_TO_LINT+=("$1")
      shift
      ;;
  esac
done

# Validate specified charts
if [ "$LINT_ALL" = false ]; then
  for chart in "${CHARTS_TO_LINT[@]}"; do
    if [ -z "${CHARTS[$chart]:-}" ]; then
      echo -e "${RED}Error: Unknown chart '$chart'${NC}"
      echo "Available charts: ${!CHARTS[@]}"
      exit 1
    fi
  done
else
  CHARTS_TO_LINT=("${!CHARTS[@]}")
fi

# Allow overriding CT_IMAGE via environment variable
if [ -n "${CT_IMAGE:-}" ]; then
  echo -e "${YELLOW}Using custom chart-testing image: ${CT_IMAGE}${NC}"
else
  CT_IMAGE="quay.io/helmpack/chart-testing:${CT_VERSION}"
fi

# Build chart-dirs arguments
CHART_DIRS_ARGS=()
for chart in "${CHARTS_TO_LINT[@]}"; do
  CHART_DIRS_ARGS+=("--chart-dirs" "${CHARTS[$chart]}")
done

# Main execution
echo -e "${GREEN}=== Linting Helm Charts ===${NC}"
echo "Repository root: ${REPO_ROOT}"
echo "Chart-testing image: ${CT_IMAGE}"
echo "Charts to lint: ${CHARTS_TO_LINT[*]}"
echo ""

echo -e "${YELLOW}Running chart-testing...${NC}"
if podman run --rm \
  -v "${REPO_ROOT}:/workspace/code" \
  -w /workspace/code \
  "${CT_IMAGE}" ct lint \
    "${CHART_DIRS_ARGS[@]}"; then
  echo ""
  echo -e "${GREEN}✓ All charts passed linting!${NC}"
  exit 0
else
  echo ""
  echo -e "${RED}✗ Chart linting failed${NC}"
  exit 1
fi
