#!/bin/bash

set -o errexit -o nounset -o pipefail

ENABLE_SSH=${ENABLE_SSH:-"true"}
if [[ "${ENABLE_SSH}" == "true" ]]; then
  if [[ ! -f /etc/ssh/ssh_host_rsa_key ]]; then
    echo "Generating SSH host keys..."
    ssh-keygen -A
  fi
  
  if [[ -n "${AUTHORIZED_KEYS:-}" ]]; then
    mkdir -p $HOME/.ssh
    chmod 700 $HOME/.ssh
    echo "${AUTHORIZED_KEYS}" > $HOME/.ssh/authorized_keys
    chmod 600 $HOME/.ssh/authorized_keys
    echo "SSH authorized keys have been configured by env: AUTHORIZED_KEYS"
  fi
  /usr/sbin/sshd -D -E /var/log/sshd.log &
fi

wait
