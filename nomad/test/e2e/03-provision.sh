#!/usr/bin/env bash
# Provisions as the operator guide does, from the runner in the operator working directory $E2E_WORK/operator:
# operator init with the Kafka super user wherry-admin, the pool e2e, the tenant e2e, the host runner and the receiver
# nomad-1, pinned to the plugin binary of wherry-receiver:e2e, then operator check. Then sets the host up from its
# bundle with wherry host init, trusting the broker's CA through --ca-cert. The runner's trust store lacks that CA, so
# the operator's operator.yaml and the host's configuration hold it as ca_cert, as the receiver's local configuration
# does.
# Last, it approves the credential request host init files, as a bundle's password works only until then, and checks
# that the next host command takes the approved password.
. "$(dirname "$0")/common.sh"
umask 077
kafka=$work/kafka
mask "$(cat "$kafka/admin-password")"

mkdir -p "$operator"
wherry operator init --dir "$operator" --broker "$(cat "$work/address"):9093" --ca-cert @"$kafka/ca.crt" \
	--super-user wherry-admin --super-user-password @"$kafka/admin-password"
mask "$(cat "$operator/operator.password")"
wherry operator apply --pool e2e --dir "$operator" --file "$e2e/pool.yaml"
wherry operator add --tenant e2e --dir "$operator"
wherry operator allow --tenant e2e --pool e2e --dir "$operator"
wherry operator add --host runner --tenant e2e --dir "$operator"
pin wherry-receiver:e2e
wherry operator check --dir "$operator"
for bundle in "$operator/bundles/hosts/e2e/runner.bundle" "$operator/bundles/receivers/nomad-1.bundle"; do
	mask "$(sed -n 's/^password: *//p' "$bundle" | tr -d "\"'")"
done

wherry host init "$operator/bundles/hosts/e2e/runner.bundle" --ca-cert @"$kafka/ca.crt"
mask "$(sed -n 's/^password: *//p' "$XDG_CONFIG_HOME/wherry/scram.next" | tr -d "\"'")"
wherry operator approve --all --dir "$operator"
host receivers >/dev/null
[ ! -e "$XDG_CONFIG_HOME/wherry/scram.next" ] || fail "the host did not take its approved password"
