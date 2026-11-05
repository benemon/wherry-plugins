#!/usr/bin/env bash
# Puts the receiver's bundle, the config.yaml with its ca_file uncommented that 03-provision.sh's pin gave receiver
# apply, and the broker's CA in the receiver's Nomad variable, as nomad/README.md does, creates the receiver's state
# volume, validates the example job and the host paths example job, runs the example job with the checkout's image, and
# checks that its receiver logs the identity line of the plugin's version e2e and the checkout's protocol. Then stops
# the job and runs it again: the new allocation finds the signing key on the volume, so the receiver starts with its key
# and logs the line again.
. "$(dirname "$0")/common.sh"
protocol=$(protocol current)
nomad var put -out none nomad/jobs/wherry-receiver bundle=@"$operator/bundles/receivers/nomad-1.bundle" \
	config=@"$work/receiver/config.yaml" ca=@"$work/kafka/ca.crt" >/dev/null
nomad volume create "$repo/examples/nomad/wherry-receiver.volume.hcl"
nomad job validate -var image=wherry-receiver:e2e "$job"
nomad job validate -var uid=1001 -var gid=1001 "$host_job"
roll wherry-receiver:e2e e2e "$protocol"

stopped() { api job/wherry-receiver/allocations | jq -e 'all(.[]; .ClientStatus != "running" and .ClientStatus != "pending")' >/dev/null; }
nomad job stop -detach wherry-receiver >/dev/null
wait_for 120 stopped
roll wherry-receiver:e2e e2e "$protocol"
echo "PASS"
