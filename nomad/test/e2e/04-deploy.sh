#!/usr/bin/env bash
# Builds the receiver image from this checkout as wherry-receiver:e2e with plugin version e2e, from the latest nomad/v*
# tag before HEAD as wherry-receiver:previous with that tag's version, both with the wherry release WHERRY_VERSION, and
# the fake agent's image. Puts the receiver's bundle in its Nomad variable, validates the example job and the host paths
# example job, runs the example job with the checkout's image, and checks that its receiver logs the plugin's version.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
previous=$(git -C "$repo" describe --tags --abbrev=0 --match 'nomad/v*' HEAD^)
echo "previous release: $previous"
docker build -f "$repo/examples/nomad/Containerfile" --build-arg VERSION=e2e --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
	-t wherry-receiver:e2e "$repo"
git -C "$repo" archive "$previous" |
	docker build -f examples/nomad/Containerfile --build-arg VERSION="${previous#nomad/}" \
		--build-arg WHERRY_VERSION="$WHERRY_VERSION" -t wherry-receiver:previous -
docker build -f "$repo/kubernetes/test/e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$repo/kubernetes/test/e2e/agent"

nomad var put -out none nomad/jobs/wherry-receiver bundle=@"$work/receiver.bundle" >/dev/null
nomad job validate -var image=wherry-receiver:e2e "$job"
nomad job validate -var version=e2e -var uid=1001 -var gid=1001 "$host_job"
roll wherry-receiver:e2e
nomad alloc logs -job wherry-receiver receiver | grep -F 'plugin nomad: version e2e ' >/dev/null ||
	fail "the receiver did not log plugin nomad: version e2e"
