#!/usr/bin/env bash
# Provisions as the operator guide does, from the runner in the operator working directory $E2E_WORK/operator:
# operator init with the Kafka super user kafka-admin, the pool e2e, the tenant e2e, the host runner and the receiver
# kind-1, whose file names the in-cluster bootstrap address and pins the plugin binary of wherry-receiver:e2e, then
# operator check. Then sets the host up from its bundle with wherry host init, trusting the Strimzi cluster CA through
# --ca-cert and with sources for a generated dummy credential and SSH private key. The runner's trust store lacks that
# CA, so the operator's operator.yaml and the host's configuration hold it as ca_cert, and the receiver's local
# configuration holds it the same way, which pin appends.
# Last, it approves the credential request host init files, as a bundle's password works only until then, and checks
# that the next host command takes the approved password.
. "$(dirname "$0")/common.sh"
umask 077
kubectl get secret -n kafka wherry-kafka-cluster-ca-cert -o jsonpath='{.data.ca\.crt}' | base64 -d >"$work/ca.crt"
kubectl get secret -n kafka kafka-admin -o jsonpath='{.data.password}' | base64 -d >"$work/kafka-admin.password"
mask "$(cat "$work/kafka-admin.password")"

mkdir -p "$operator"
wherry operator init --dir "$operator" --broker localhost:31092 --ca-cert @"$work/ca.crt" \
	--super-user kafka-admin --super-user-password @"$work/kafka-admin.password"
mask "$(cat "$operator/operator.password")"
wherry operator apply --pool e2e --dir "$operator" --file "$e2e/pool.yaml"
wherry operator add --tenant e2e --dir "$operator"
wherry operator allow --tenant e2e --pool e2e --dir "$operator"
wherry operator add --host runner --tenant e2e --dir "$operator"
pin wherry-receiver:e2e
wherry operator check --dir "$operator"
for bundle in "$operator/bundles/hosts/e2e/runner.bundle" "$operator/bundles/receivers/kind-1.bundle"; do
	mask "$(sed -n 's/^password: *//p' "$bundle" | tr -d "\"'")"
done

mkdir -p "$work/host" "$(dirname "$host_config")"
head -c 24 /dev/urandom | base64 >"$work/host/dummy"
mask "$(cat "$work/host/dummy")"
ssh-keygen -q -t ed25519 -N '' -C wherry-e2e -f "$work/host/deploy-key"
# The host's own sources for the credentials 05-runs.sh names; host init keeps them.
cat >"$host_config" <<YAML
credentials:
  dummy: {file: "$work/host/dummy"}
  deploy: {file: "$work/host/deploy-key"}
YAML
wherry host init "$operator/bundles/hosts/e2e/runner.bundle" --ca-cert @"$work/ca.crt"
mask "$(sed -n 's/^password: *//p' "$XDG_CONFIG_HOME/wherry/scram.next" | tr -d "\"'")"
wherry operator approve --all --dir "$operator"
host receivers >/dev/null
[ ! -e "$XDG_CONFIG_HOME/wherry/scram.next" ] || fail "the host did not take its approved password"
