#!/usr/bin/env bash
# Harness: makes ready what Run the receiver as a systemd service starts with, as the operator guide does, from the
# runner in the operator working directory $E2E_WORK/operator: operator init with the Kafka super user wherry-admin,
# the pool e2e, and the tenant e2e entitled to it with the host runner. Then sets the host up from its bundle with
# wherry host init, trusting the broker's CA through --ca-cert, as the Nomad lane does: the runner's trust store lacks
# it, so the operator's operator.yaml and the host's configuration hold it as ca_cert.
. "$(dirname "$0")/common.sh"
umask 077
kafka=$work/kafka
mask "$(cat "$kafka/admin-password")"

mkdir -p "$operator"
wherry operator init --dir "$operator" --broker 127.0.0.1:9093 --ca-cert @"$kafka/ca.crt" \
	--super-user wherry-admin --super-user-password @"$kafka/admin-password"
mask "$(cat "$operator/operator.password")"
wherry operator apply --pool e2e --dir "$operator" --file "$e2e/pool.yaml"
wherry operator add --tenant e2e --dir "$operator"
wherry operator allow --tenant e2e --pool e2e --dir "$operator"
wherry operator add --host runner --tenant e2e --dir "$operator"
mask "$(sed -n 's/^password: *//p' "$operator/bundles/hosts/e2e/runner.bundle" | tr -d "\"'")"

wherry host init "$operator/bundles/hosts/e2e/runner.bundle" --ca-cert @"$kafka/ca.crt"
