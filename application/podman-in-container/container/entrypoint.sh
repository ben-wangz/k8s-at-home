#!/bin/bash

set -o errexit -o nounset -o pipefail

# SSH setup
ENABLE_SSH=${ENABLE_SSH:-"true"}
if [[ "${ENABLE_SSH}" == "true" ]]; then
  mkdir -p $HOME/.ssh
  chmod 700 $HOME $HOME/.ssh
  if [[ ! -f /etc/ssh/ssh_host_rsa_key ]]; then
    echo "Generating SSH host keys..."
    ssh-keygen -A
  fi
  if [[ -n "${AUTHORIZED_KEYS:-}" ]]; then
    echo "${AUTHORIZED_KEYS}" > $HOME/.ssh/authorized_keys
    chmod 600 $HOME/.ssh/authorized_keys
    echo "SSH authorized keys have been configured by env: AUTHORIZED_KEYS"
  fi
  /usr/sbin/sshd -D -E /var/log/sshd.log &
fi

# OpenVSCode Server setup
if [[ -n "${OPEN_VSCODE_SERVER_CONNECTION_TOKEN:-}" ]]; then
  echo "OpenVSCode Server is enabled (OPEN_VSCODE_SERVER_CONNECTION_TOKEN is set)"

  # Set default values for OpenVSCode Server configuration
  OPEN_VSCODE_SERVER_PORT=${OPEN_VSCODE_SERVER_PORT:-3000}
  OPEN_VSCODE_SERVER_LISTENING_HOST=${OPEN_VSCODE_SERVER_LISTENING_HOST:-0.0.0.0}

  # Start OpenVSCode Server in background
  /opt/openvscode-server/current/bin/openvscode-server \
    --port ${OPEN_VSCODE_SERVER_PORT} \
    --connection-token ${OPEN_VSCODE_SERVER_CONNECTION_TOKEN} \
    --host ${OPEN_VSCODE_SERVER_LISTENING_HOST} &

  echo "OpenVSCode Server started on ${OPEN_VSCODE_SERVER_LISTENING_HOST}:${OPEN_VSCODE_SERVER_PORT}"
else
  echo "OpenVSCode Server is disabled (OPEN_VSCODE_SERVER_CONNECTION_TOKEN not set)"
fi

wait
