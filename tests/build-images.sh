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

# Check if podman is available
if ! command -v podman &> /dev/null; then
  echo -e "${RED}Error: podman could not be found${NC}"
  echo "Please install podman first"
  exit 1
fi

# Define images to build (matching the GitHub Actions workflow)
declare -A IMAGES=(
  ["aria2"]="application/aria2/container/aria2"
  ["aria-ng"]="application/aria2/container/aria-ng"
  ["podman-in-container"]="application/podman-in-container/container"
)

# Parse command line arguments
BUILD_ALL=true
IMAGES_TO_BUILD=()
TAG_PREFIX="test"
PUSH=false

usage() {
  cat << EOF
Usage: $(basename "$0") [OPTIONS] [IMAGES...]

Build container images for testing before committing code.

OPTIONS:
  -t, --tag TAG       Tag prefix for built images (default: test)
  -p, --push          Push images after building (default: false)
  -h, --help          Show this help message

IMAGES:
  If no images specified, all images will be built.
  Available images: ${!IMAGES[@]}

ENVIRONMENT VARIABLES:
  You can override Dockerfile ARG values via environment variables:
    ALPINE_IMAGE          - Base Alpine image (default: m.daocloud.io/docker.io/library/alpine:latest)
    NGINX_IMAGE           - Base Nginx image (default: m.daocloud.io/docker.io/library/nginx:stable)
    PODMAN_IMAGE          - Base Podman image (default: m.daocloud.io/quay.io/podman/stable:v5.6.1)
    ARIA_NG_DOWNLOAD_URL  - AriaNg download URL
    TINI_VERSION          - Tini init system version (default: v0.19.0)

EXAMPLES:
  # Build all images
  $(basename "$0")

  # Build specific images
  $(basename "$0") aria2 aria-ng

  # Build with custom tag
  $(basename "$0") --tag dev aria2

  # Build and push
  $(basename "$0") --push --tag latest

  # Build with mirrored base images (for private environments)
  ALPINE_IMAGE=m.daocloud.io/docker.io/library/alpine:latest $(basename "$0") aria2

  # Build aria-ng with both mirrored base image and custom download URL
  NGINX_IMAGE=m.daocloud.io/docker.io/library/nginx:stable \\
  ARIA_NG_DOWNLOAD_URL=https://mirror.example.com/AriaNg-1.3.10.zip \\
    $(basename "$0") aria-ng

  # Build all images with mirrored registries
  ALPINE_IMAGE=m.daocloud.io/docker.io/library/alpine:latest \\
  NGINX_IMAGE=m.daocloud.io/docker.io/library/nginx:stable \\
    $(basename "$0")

EOF
}

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    -t|--tag)
      TAG_PREFIX="$2"
      shift 2
      ;;
    -p|--push)
      PUSH=true
      shift
      ;;
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
      BUILD_ALL=false
      IMAGES_TO_BUILD+=("$1")
      shift
      ;;
  esac
done

# Validate specified images
if [ "$BUILD_ALL" = false ]; then
  for img in "${IMAGES_TO_BUILD[@]}"; do
    if [ -z "${IMAGES[$img]:-}" ]; then
      echo -e "${RED}Error: Unknown image '$img'${NC}"
      echo "Available images: ${!IMAGES[@]}"
      exit 1
    fi
  done
else
  IMAGES_TO_BUILD=("${!IMAGES[@]}")
fi

# Collect build args from environment variables
collect_build_args() {
  local build_args=()

  # Common build args that can be overridden via environment variables
  local common_args=(
    "ALPINE_IMAGE"
    "NGINX_IMAGE"
    "PODMAN_IMAGE"
    "ARIA_NG_DOWNLOAD_URL"
    "TINI_VERSION"
  )

  for arg in "${common_args[@]}"; do
    if [ -n "${!arg:-}" ]; then
      build_args+=("--build-arg" "${arg}=${!arg}")
    fi
  done

  echo "${build_args[@]}"
}

# Validate semver format
validate_semver() {
  local version=$1
  local name=$2

  # Semver regex: X.Y.Z with optional pre-release and build metadata
  # Supports: 1.0.0, 2.1.3-alpha.1, 1.0.0-beta+exp.sha.5114f85, etc.
  local semver_regex='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*))?(\+([0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*))?$'

  if ! echo "${version}" | grep -Eq "${semver_regex}"; then
    echo -e "${RED}Error: VERSION file for '${name}' contains invalid semver version: ${version}${NC}"
    echo "Expected format: X.Y.Z (e.g., 1.0.0, 2.1.3, 1.0.0-alpha.1, 1.0.0+build.123)"
    echo "See https://semver.org for details"
    return 1
  fi

  return 0
}

# Build function
build_image() {
  local name=$1
  local context_path="${REPO_ROOT}/${IMAGES[$name]}"
  local dockerfile="${context_path}/Dockerfile"
  local version_file="${context_path}/VERSION"

  # Read version from VERSION file if using default tag
  if [ "$TAG_PREFIX" = "test" ] && [ -f "$version_file" ]; then
    local version=$(cat "$version_file" | tr -d '[:space:]')

    # Validate semver format
    if ! validate_semver "$version" "$name"; then
      return 1
    fi

    local image_tag="k8s-at-home-${name}:${version}"
    echo -e "${YELLOW}Building ${name} (version: ${version})...${NC}"
  else
    local image_tag="k8s-at-home-${name}:${TAG_PREFIX}"
    echo -e "${YELLOW}Building ${name}...${NC}"
  fi

  echo "  Context: ${context_path}"
  echo "  Dockerfile: ${dockerfile}"
  echo "  Tag: ${image_tag}"

  if [ ! -f "$dockerfile" ]; then
    echo -e "${RED}Error: Dockerfile not found at ${dockerfile}${NC}"
    return 1
  fi

  # Collect build args from environment
  local build_args_array
  read -ra build_args_array <<< "$(collect_build_args)"

  # Display build args if any
  if [ ${#build_args_array[@]} -gt 0 ]; then
    echo "  Build args:"
    for ((i=0; i<${#build_args_array[@]}; i+=2)); do
      if [ "${build_args_array[$i]}" = "--build-arg" ]; then
        echo "    ${build_args_array[$((i+1))]}"
      fi
    done
  fi

  # Build the image with collected build args
  if podman build -t "${image_tag}" -f "${dockerfile}" "${build_args_array[@]}" "${context_path}"; then
    echo -e "${GREEN}✓ Successfully built ${image_tag}${NC}"

    # Push if requested
    if [ "$PUSH" = true ]; then
      echo -e "${YELLOW}Pushing ${image_tag}...${NC}"
      if podman push "${image_tag}"; then
        echo -e "${GREEN}✓ Successfully pushed ${image_tag}${NC}"
      else
        echo -e "${RED}✗ Failed to push ${image_tag}${NC}"
        return 1
      fi
    fi

    return 0
  else
    echo -e "${RED}✗ Failed to build ${image_tag}${NC}"
    return 1
  fi
}

# Main execution
echo -e "${GREEN}=== Building Container Images ===${NC}"
echo "Repository root: ${REPO_ROOT}"
echo "Tag prefix: ${TAG_PREFIX}"
echo "Images to build: ${IMAGES_TO_BUILD[*]}"
echo ""

FAILED_IMAGES=()
SUCCESSFUL_IMAGES=()

for img in "${IMAGES_TO_BUILD[@]}"; do
  if build_image "$img"; then
    SUCCESSFUL_IMAGES+=("$img")
  else
    FAILED_IMAGES+=("$img")
  fi
  echo ""
done

# Summary
echo -e "${GREEN}=== Build Summary ===${NC}"
echo -e "${GREEN}Successful (${#SUCCESSFUL_IMAGES[@]}):${NC}"
for img in "${SUCCESSFUL_IMAGES[@]}"; do
  # Determine what tag was used
  local context_path="${REPO_ROOT}/${IMAGES[$img]}"
  local version_file="${context_path}/VERSION"
  if [ "$TAG_PREFIX" = "test" ] && [ -f "$version_file" ]; then
    local version=$(cat "$version_file" | tr -d '[:space:]')
    echo -e "  ${GREEN}✓${NC} k8s-at-home-${img}:${version}"
  else
    echo -e "  ${GREEN}✓${NC} k8s-at-home-${img}:${TAG_PREFIX}"
  fi
done

if [ ${#FAILED_IMAGES[@]} -gt 0 ]; then
  echo ""
  echo -e "${RED}Failed (${#FAILED_IMAGES[@]}):${NC}"
  for img in "${FAILED_IMAGES[@]}"; do
    echo -e "  ${RED}✗${NC} k8s-at-home-${img}:${TAG_PREFIX}"
  done
  exit 1
fi

echo ""
echo -e "${GREEN}All images built successfully!${NC}"
