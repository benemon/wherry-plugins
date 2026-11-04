#!/usr/bin/env bash
# Puts the receiver's bundle, examples/nomad/config.yaml with its ca_file uncommented, and the broker's CA in the
# receiver's Nomad variable, as nomad/README.md does, creates the receiver's state volume, validates the example job
# and the host paths example job, runs the example job with the checkout's image, and checks that its receiver logs
# the plugin's version e2e. Then stops the job and runs it again: the new allocation finds the signing key on the
# volume, so the receiver starts with its key and logs the version again.
. "$(dirname "$0")/common.sh"
sed 's|^# ca_file: |ca_file: |' "$repo/examples/nomad/config.yaml" >"$work/config.yaml"
grep -x 'ca_file: /secrets/kafka-ca.pem' "$work/config.yaml" >/dev/null || fail "config.yaml names no ca_file"
nomad var put -out none nomad/jobs/wherry-receiver bundle=@"$operator/bundles/receivers/nomad-1.bundle" \
	config=@"$work/config.yaml" ca=@"$work/kafka/ca.crt" >/dev/null
nomad volume create "$repo/examples/nomad/wherry-receiver.volume.hcl"
nomad job validate -var image=wherry-receiver:e2e "$job"
nomad job validate -var uid=1001 -var gid=1001 "$host_job"
roll wherry-receiver:e2e e2e

stopped() { api job/wherry-receiver/allocations | jq -e 'all(.[]; .ClientStatus != "running" and .ClientStatus != "pending")' >/dev/null; }
nomad job stop -detach wherry-receiver >/dev/null
wait_for 120 stopped
roll wherry-receiver:e2e e2e
echo "PASS"
