#!/bin/bash

set -o errexit -o nounset -o pipefail

ENABLE_SSH=${ENABLE_SSH:-"true"}
if [[ "${ENABLE_SSH}" == "true" ]]; then
  if [[ -n "${AUTHORIZED_KEYS:-}" ]]; then
    mkdir -p $HOME/.ssh
    chmod 700 $HOME/.ssh
    echo "${AUTHORIZED_KEYS}" > $HOME/.ssh/authorized_keys
    chmod 600 $HOME/.ssh/authorized_keys
    echo "SSH authorized keys have been configured by env: AUTHORIZED_KEYS"
  fi
  /usr/sbin/sshd -D &
fi

wait
