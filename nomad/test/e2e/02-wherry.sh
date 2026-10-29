#!/usr/bin/env bash
# Fetches the wherry release WHERRY_VERSION for this machine into $E2E_WORK/bin, through the receiver image's own
# fetch stage, which verifies the checksums' cosign signature and the archive's SHA-256. Builds the receiver image from
# this checkout as wherry-receiver:e2e with plugin version e2e, from the latest nomad/v* tag before HEAD as
# wherry-receiver:previous with that tag's version, both with WHERRY_VERSION, and the fake agent's image: provisioning
# pins the plugin binary of the image the receiver runs.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "unsupported machine $(uname -m)" >&2 && exit 1 ;;
esac
docker build -f "$repo/examples/nomad/Containerfile" --target wherry --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
	--build-arg WHERRY_ARCH="$arch" --output "type=local,dest=$work/bin" "$repo"

previous=$(git -C "$repo" describe --tags --abbrev=0 --match 'nomad/v*' HEAD^)
echo "previous release: $previous"
docker build -f "$repo/examples/nomad/Containerfile" --build-arg VERSION=e2e --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
	-t wherry-receiver:e2e "$repo"
git -C "$repo" archive "$previous" |
	docker build -f examples/nomad/Containerfile --build-arg VERSION="${previous#nomad/}" \
		--build-arg WHERRY_VERSION="$WHERRY_VERSION" -t wherry-receiver:previous -
docker build -f "$repo/kubernetes/test/e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$repo/kubernetes/test/e2e/agent"
