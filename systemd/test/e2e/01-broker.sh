#!/usr/bin/env bash
# Harness: starts a single-broker Kafka with TLS and SCRAM on 127.0.0.1:9093 through test/kafka/up.sh of the wherry
# release WHERRY_VERSION, pinned by SHA-256 as the Nomad lane pins it, with the Kafka super user wherry-admin. Needs
# docker.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
up_sha256=278838703acc20922afc9dbeae1ff67ada4fc020048cfc0b510b3122f0bb6d41

rm -rf "$work"
mkdir -p "$work/bin"
curl -fsSL -o "$work/up.sh" "https://raw.githubusercontent.com/benemon/wherry/$WHERRY_VERSION/test/kafka/up.sh"
echo "$up_sha256  $work/up.sh" | sha256sum -c -
bash "$work/up.sh" "$work/kafka"
