#! /bin/bash

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
TARGET_IMAGE=${TARGET_IMAGE:-aria-ng:latest}
NGINX_IMAGE=${NGINX_IMAGE:-m.daocloud.io/docker.io/library/nginx:stable}

ARIA_NG_DOWNLOAD_URL=https://github.com/mayswind/AriaNg/releases/download/1.3.10/AriaNg-1.3.10.zip
podman build \
  --tag ${TARGET_IMAGE} \
  --build-arg NGINX_IMAGE=${NGINX_IMAGE} \
  --build-arg ARIA_NG_DOWNLOAD_URL=${ARIA_NG_DOWNLOAD_URL} \
  --file $SCRIPT_DIR/Dockerfile \
  $SCRIPT_DIR
