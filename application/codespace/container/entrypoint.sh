#!/bin/bash

set -o errexit -o nounset -o pipefail

configure_podman_network() {
    local cni_config=/etc/cni/net.d/87-podman-bridge.conflist
    local subnet=${PODMAN_NETWORK_SUBNET:-10.250.0.0/16}
    local gateway=${PODMAN_NETWORK_GATEWAY:-}
    local tmp_config

    gateway="$(python3 - "${subnet}" "${gateway}" <<'PY'
import ipaddress
import sys

raw_subnet, raw_gateway = sys.argv[1:]

try:
    network = ipaddress.ip_network(raw_subnet, strict=True)
except ValueError as error:
    raise SystemExit(f"Invalid PODMAN_NETWORK_SUBNET: {error}")

if network.version != 4:
    raise SystemExit("PODMAN_NETWORK_SUBNET must be an IPv4 network")
if network.prefixlen > 30:
    raise SystemExit("PODMAN_NETWORK_SUBNET must provide space for a gateway and containers")

if raw_gateway:
    try:
        gateway = ipaddress.ip_address(raw_gateway)
    except ValueError as error:
        raise SystemExit(f"Invalid PODMAN_NETWORK_GATEWAY: {error}")
else:
    gateway = next(network.hosts())

if gateway.version != 4 or gateway not in network:
    raise SystemExit("PODMAN_NETWORK_GATEWAY must be an IPv4 address inside PODMAN_NETWORK_SUBNET")
if gateway in (network.network_address, network.broadcast_address):
    raise SystemExit("PODMAN_NETWORK_GATEWAY cannot be the network or broadcast address")

print(gateway)
PY
)"

    tmp_config="$(mktemp "${cni_config}.tmp.XXXXXX")"
    jq \
        --arg subnet "${subnet}" \
        --arg gateway "${gateway}" \
        '(.plugins[] | select(.type == "bridge") | .ipam.ranges[0][0].subnet) = $subnet
         | (.plugins[] | select(.type == "bridge") | .ipam.ranges[0][0].gateway) = $gateway' \
        "${cni_config}" > "${tmp_config}"
    install -m 0644 "${tmp_config}" "${cni_config}"
    rm -f "${tmp_config}"

    echo "Podman network configured: subnet=${subnet}, gateway=${gateway}"
}

configure_podman_network

if [[ ! -f /root/.bash_profile ]]; then
    printf '%s\n' \
        'if [[ -f ~/.bashrc ]]; then' \
        '    source ~/.bashrc' \
        'fi' \
        > /root/.bash_profile
    chmod 0644 /root/.bash_profile
    echo "Created /root/.bash_profile"
fi

install -d -m 0755 /run/sshd
ssh-keygen -A

install -d -m 0700 /root/.ssh
if [[ -n "${AUTHORIZED_KEYS:-}" ]]; then
    printf '%s\n' "${AUTHORIZED_KEYS}" > /root/.ssh/authorized_keys
fi

if [[ -f /root/.ssh/authorized_keys ]]; then
    chmod 0600 /root/.ssh/authorized_keys
fi

exec /usr/sbin/sshd -D -e
