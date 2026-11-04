#!/usr/bin/env bash
# Harness: makes ready what Run the receiver as a systemd service starts with, as the operator guide does, from the
# runner in the operator working directory $E2E_WORK/operator: operator init with the Kafka super user wherry-admin,
# the pool e2e, and the tenant e2e entitled to it with the host runner. Then sets the host up from its bundle with
# wherry host init, trusting the broker's CA through $XDG_CONFIG_HOME/wherry/ca.pem, as the Nomad lane does.
. "$(dirname "$0")/common.sh"
umask 077
kafka=$work/kafka
mask "$(cat "$kafka/admin-password")"

mkdir -p "$operator"
wherry operator init --dir "$operator" --broker 127.0.0.1:9093 --ca-file "$kafka/ca.crt" \
	--super-user wherry-admin --super-user-password-file "$kafka/admin-password"
mask "$(cat "$operator/operator.password")"
wherry operator pool apply e2e --dir "$operator" -f "$e2e/pool.yaml"
wherry operator tenant add e2e --dir "$operator"
wherry operator tenant pool add e2e e2e --dir "$operator"
wherry operator tenant host add e2e runner --dir "$operator"
mask "$(sed -n 's/^password: *//p' "$operator/bundles/hosts/e2e/runner.bundle" | tr -d "\"'")"

mkdir -p "$XDG_CONFIG_HOME/wherry"
cp "$kafka/ca.crt" "$XDG_CONFIG_HOME/wherry/ca.pem"
wherry host init "$operator/bundles/hosts/e2e/runner.bundle"
