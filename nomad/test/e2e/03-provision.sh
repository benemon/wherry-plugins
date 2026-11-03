#!/usr/bin/env bash
# Provisions as the operator guide does, from the runner in the operator working directory $E2E_WORK/operator:
# operator init with the Kafka super user wherry-admin, the pool e2e, the tenant e2e, the host runner and the receiver
# nomad-1, pinned to the plugin binary of wherry-receiver:e2e, then operator check. Then sets the host up from its
# bundle with wherry host init, trusting the broker's CA through ca.pem.
. "$(dirname "$0")/common.sh"
umask 077
kafka=$work/kafka
mask "$(cat "$kafka/admin-password")"

mkdir -p "$operator"
wherry operator init --dir "$operator" --broker "$(cat "$work/address"):9093" --ca-file "$kafka/ca.crt" \
	--super-user wherry-admin --super-user-password-file "$kafka/admin-password"
mask "$(cat "$operator/operator.password")"
wherry operator pool apply e2e --dir "$operator" -f "$e2e/pool.yaml"
wherry operator tenant add e2e --dir "$operator"
wherry operator tenant pool add e2e e2e --dir "$operator"
wherry operator tenant host add e2e runner --dir "$operator"
pin wherry-receiver:e2e
wherry operator check --dir "$operator"
for bundle in "$operator/bundles/hosts/e2e/runner.bundle" "$operator/bundles/receivers/nomad-1.bundle"; do
	mask "$(sed -n 's/^password: *//p' "$bundle" | tr -d "\"'")"
done

mkdir -p "$XDG_CONFIG_HOME/wherry"
cp "$kafka/ca.crt" "$XDG_CONFIG_HOME/wherry/ca.pem"
wherry host init "$operator/bundles/hosts/e2e/runner.bundle"
