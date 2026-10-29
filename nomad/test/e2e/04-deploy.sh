#!/usr/bin/env bash
# Puts the receiver's bundle in its Nomad variable, validates the example job and the host paths example job, runs the
# example job with the checkout's image, and checks that its receiver logs the plugin's version.
. "$(dirname "$0")/common.sh"
nomad var put -out none nomad/jobs/wherry-receiver bundle=@"$work/receiver.bundle" >/dev/null
nomad job validate -var image=wherry-receiver:e2e "$job"
nomad job validate -var version=e2e -var uid=1001 -var gid=1001 "$host_job"
roll wherry-receiver:e2e
nomad alloc logs -job wherry-receiver receiver | grep -F 'plugin nomad: version e2e ' >/dev/null ||
	fail "the receiver did not log plugin nomad: version e2e"
