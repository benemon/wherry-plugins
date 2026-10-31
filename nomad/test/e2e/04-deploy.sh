#!/usr/bin/env bash
# Puts the receiver's bundle in its Nomad variable, creates the receiver's state volume, validates the example job and
# the host paths example job, runs the example job with the checkout's image, and checks that its receiver logs the
# plugin's version. Then stops the job and runs it again: the new allocation finds the signing key on the volume, so
# the receiver starts with its key.
. "$(dirname "$0")/common.sh"
nomad var put -out none nomad/jobs/wherry-receiver bundle=@"$work/receiver.bundle" >/dev/null
nomad volume create "$repo/examples/nomad/wherry-receiver.volume.hcl"
nomad job validate -var image=wherry-receiver:e2e "$job"
nomad job validate -var version=e2e -var uid=1001 -var gid=1001 "$host_job"
roll wherry-receiver:e2e
nomad alloc logs -job wherry-receiver receiver | grep -F 'plugin nomad: version e2e ' >/dev/null ||
	fail "the receiver did not log plugin nomad: version e2e"

stopped() { api job/wherry-receiver/allocations | jq -e 'all(.[]; .ClientStatus != "running" and .ClientStatus != "pending")' >/dev/null; }
nomad job stop -detach wherry-receiver >/dev/null
wait_for 120 stopped
roll wherry-receiver:e2e
alloc=$(api job/wherry-receiver/allocations | jq -r \
	'map(select(.ClientStatus == "running")) | sort_by(.CreateIndex) | last | .ID // empty')
[ -n "$alloc" ] || fail "the receiver job has no running allocation after a stop and run"
nomad alloc logs "$alloc" receiver | grep -F 'plugin nomad: version e2e ' >/dev/null ||
	fail "the receiver did not start after a stop and run"
echo "PASS"
