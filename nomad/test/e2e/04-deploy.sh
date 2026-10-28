#!/usr/bin/env bash
# Builds the receiver image from this checkout as wherry-receiver:e2e, from the latest nomad/v* tag before HEAD as
# wherry-receiver:previous, both with the wherry release WHERRY_VERSION, and the fake agent's image. Puts the
# receiver's bundle in its Nomad variable, validates the example job and runs it with the checkout's image.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
previous=$(git -C "$repo" describe --tags --abbrev=0 --match 'nomad/v*' HEAD^)
echo "previous release: $previous"
docker build -f "$repo/examples/nomad/Containerfile" --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
	-t wherry-receiver:e2e "$repo"
git -C "$repo" archive "$previous" |
	docker build -f examples/nomad/Containerfile --build-arg WHERRY_VERSION="$WHERRY_VERSION" -t wherry-receiver:previous -
docker build -f "$repo/kubernetes/test/e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$repo/kubernetes/test/e2e/agent"

nomad var put -out none nomad/jobs/wherry-receiver bundle=@"$work/receiver.bundle" >/dev/null
nomad job validate -var image=wherry-receiver:e2e "$job"
roll wherry-receiver:e2e
