#!/usr/bin/env bash
# Starts a Nomad agent in dev mode as the systemd unit wherry-nomad, replacing one of that name, with the docker driver,
# and a single-broker Kafka with TLS and SCRAM through test/kafka/up.sh of the wherry release WHERRY_VERSION, pinned by
# SHA-256. The broker advertises the runner's address on its default route, which the receiver's bridge network
# reaches. Needs root through sudo, systemd, docker, nomad and the CNI reference plugins in /opt/cni/bin.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
up_sha256=366cda461e604733496b6656ecc2097f16bcb0fafb2531646dfd35a32f0722ad

sudo systemctl stop wherry-nomad 2>/dev/null || true
sudo systemctl reset-failed wherry-nomad 2>/dev/null || true
rm -rf "$work"
mkdir -p "$work/bin"
# Templates set the receiver's bundle owner, which needs the agent to run as root.
sudo systemd-run --unit wherry-nomad "$(command -v nomad)" agent -dev -config "$e2e/agent.hcl"
# The client is ready once docker is healthy and it has fingerprinted the CNI plugins bridge mode runs.
client_ready() {
	local node
	node=$(api nodes | jq -r '.[0] | select(.Status == "ready" and .Drivers.docker.Healthy) | .ID')
	[ -n "$node" ] && api "node/$node" |
		jq -e '.Attributes | has("plugins.cni.version.bridge") and has("plugins.cni.version.portmap")' >/dev/null
}
wait_for 120 client_ready

curl -fsSL -o "$work/up.sh" "https://raw.githubusercontent.com/benemon/wherry/$WHERRY_VERSION/test/kafka/up.sh"
echo "$up_sha256  $work/up.sh" | sha256sum -c -
address=$(ip -4 -o route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
[ -n "$address" ] || fail "the runner has no address on a default route"
ADVERTISED_IP=$address bash "$work/up.sh" "$work/kafka"
