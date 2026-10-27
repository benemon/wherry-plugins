#!/usr/bin/env bash
# Provisions as the operator guide does, with wherry admin from the runner: the pool e2e, the tenant e2e, the host
# runner and the receiver kind-1. Then writes the host's client configuration, with a generated dummy credential,
# and registers the host key.
. "$(dirname "$0")/common.sh"
umask 077
kubectl get secret -n kafka wherry-kafka-cluster-ca-cert -o jsonpath='{.data.ca\.crt}' | base64 -d >"$work/ca.crt"
for user in kafka-admin wherry.operator; do
	kubectl get secret -n kafka "$user" -o jsonpath='{.data.password}' | base64 -d >"$work/$user.password"
	mask "$(cat "$work/$user.password")"
done
export WHERRY_TRUST_BROKER=localhost:31092 WHERRY_TRUST_CA=$work/ca.crt \
	WHERRY_TRUST_ADMIN_USER=kafka-admin WHERRY_TRUST_ADMIN_PASSWORD_FILE=$work/kafka-admin.password \
	WHERRY_TRUST_OPERATOR_USER=wherry.operator WHERRY_TRUST_OPERATOR_PASSWORD_FILE=$work/wherry.operator.password

wherry admin pool create e2e --config-file "$e2e/pool.yaml"
wherry admin pool entitle e2e --tenant e2e
wherry admin host add runner --tenant e2e --bundle-file "$work/host.bundle"
wherry admin receiver add kind-1 --pool e2e --config-dir "$e2e/receiver" --bundle-file "$work/receiver.bundle"
for bundle in host receiver; do
	mask "$(sed -n 's/^password: *//p' "$work/$bundle.bundle" | tr -d "\"'")"
done

mkdir -p "$work/host"
head -c 24 /dev/urandom | base64 >"$work/host/dummy"
mask "$(cat "$work/host/dummy")"
cat >"$host_config" <<YAML
bundle: "$work/host.bundle"
host_key: "$work/host/host.key"
state_dir: "$work/host/state"
credentials:
  dummy: {file: "$work/host/dummy"}
YAML
wherry init --config-file "$host_config"
