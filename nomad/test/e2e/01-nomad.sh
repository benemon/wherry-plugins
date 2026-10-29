#!/usr/bin/env bash
# Starts a Nomad agent in dev mode as the systemd unit wherry-nomad, replacing one of that name, with the docker driver,
# and a single-broker Kafka with TLS and SCRAM through test/kafka/up.sh of the wherry release WHERRY_VERSION, pinned by
# SHA-256. Needs root through sudo, systemd, docker and nomad.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
up_sha256=366cda461e604733496b6656ecc2097f16bcb0fafb2531646dfd35a32f0722ad

sudo systemctl stop wherry-nomad 2>/dev/null || true
sudo systemctl reset-failed wherry-nomad 2>/dev/null || true
rm -rf "$work"
mkdir -p "$work/bin"
# Templates set the receiver's bundle owner, which needs the agent to run as root.
sudo systemd-run --unit wherry-nomad "$(command -v nomad)" agent -dev -config "$e2e/agent.hcl"
docker_healthy() { api nodes | jq -e '.[0].Status == "ready" and .[0].Drivers.docker.Healthy' >/dev/null; }
wait_for 120 docker_healthy

curl -fsSL -o "$work/up.sh" "https://raw.githubusercontent.com/benemon/wherry/$WHERRY_VERSION/test/kafka/up.sh"
echo "$up_sha256  $work/up.sh" | sha256sum -c -
bash "$work/up.sh" "$work/kafka"
