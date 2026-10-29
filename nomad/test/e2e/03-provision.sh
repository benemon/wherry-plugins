#!/usr/bin/env bash
# Provisions as the operator guide does, with wherry admin from the runner: the pool e2e, the tenant e2e, the host
# runner and the receiver nomad-1, pinned to the plugin binary of wherry-receiver:e2e, and checks the result with wherry
# admin check, which reports a tenant feed below the 2 MiB record limit. Then writes the host's client configuration
# and registers the host key.
. "$(dirname "$0")/common.sh"
umask 077
# shellcheck source=/dev/null
. "$work/kafka/env"
for file in "$WHERRY_TRUST_ADMIN_PASSWORD_FILE" "$WHERRY_TRUST_OPERATOR_PASSWORD_FILE"; do mask "$(cat "$file")"; done

wherry admin pool create e2e --config-file "$e2e/pool.yaml"
wherry admin pool entitle e2e --tenant e2e
wherry admin host add runner --tenant e2e --bundle-file "$work/host.bundle"
wherry admin receiver add nomad-1 --pool e2e --config-dir "$e2e/receiver" \
	--plugin-sha256 "nomad=$(plugin_sha256 wherry-receiver:e2e)" --bundle-file "$work/receiver.bundle"
wherry admin check
for bundle in host receiver; do
	mask "$(sed -n 's/^password: *//p' "$work/$bundle.bundle" | tr -d "\"'")"
done

mkdir -p "$work/host"
cat >"$host_config" <<YAML
bundle: "$work/host.bundle"
host_key: "$work/host/host.key"
state_dir: "$work/host/state"
YAML
wherry init --config-file "$host_config"
