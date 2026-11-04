#!/usr/bin/env bash
# Fetches the wherry release WHERRY_VERSION for this machine into $E2E_WORK/bin, through the receiver image's own
# fetch stage, which verifies the checksums' cosign signature and the archive's SHA-256. Builds the receiver image from
# this checkout as wherry-receiver:e2e with plugin version e2e, from the latest nomad/v* tag before HEAD as
# wherry-receiver:previous with that tag's version, the tag less its nomad/ prefix, which it keeps in
# $E2E_WORK/previous-version, both with WHERRY_VERSION, and the fake agent's image: provisioning pins the plugin binary
# of the image the receiver runs. First it runs the plugin protocol check, which finds the tag and keeps its result for
# the later steps. With no nomad/v* tag before HEAD, or any other failure of the check, it fails.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "unsupported machine $(uname -m)" >&2 && exit 1 ;;
esac
docker build -f "$repo/examples/nomad/Containerfile" --target wherry --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
	--build-arg WHERRY_ARCH="$arch" --output "type=local,dest=$work/bin" "$repo"

protocol_check
previous=$(checked previous)
previous_version=${previous#nomad/}
echo "previous release: $previous, plugin version $previous_version"
echo "$previous_version" >"$work/previous-version"
docker build -f "$repo/examples/nomad/Containerfile" --build-arg VERSION=e2e --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
	-t wherry-receiver:e2e "$repo"
git -C "$repo" archive "$previous" |
	docker build -f examples/nomad/Containerfile --build-arg VERSION="$previous_version" \
		--build-arg WHERRY_VERSION="$WHERRY_VERSION" -t wherry-receiver:previous -
docker build -f "$repo/kubernetes/test/e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$repo/kubernetes/test/e2e/agent"
