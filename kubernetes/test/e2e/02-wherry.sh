#!/usr/bin/env bash
# Fetches the wherry release WHERRY_VERSION for this machine into $E2E_WORK/bin, through the receiver image's own
# fetch stage, which verifies the checksums' cosign signature and the archive's SHA-256. Builds the receiver image with
# make image as wherry-receiver:e2e with plugin version e2e, from the latest kubernetes/v* tag before HEAD as
# wherry-receiver:previous with that tag's version, which it keeps in $E2E_WORK/previous-version, both with
# WHERRY_VERSION, and the fake agent's image: provisioning pins the plugin binary of the image the receiver runs. With
# no kubernetes/v* tag before HEAD it fails, as the upgrade has no release to start from.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "unsupported machine $(uname -m)" >&2 && exit 1 ;;
esac
docker build -f "$repo/examples/kubernetes/Containerfile" --target wherry --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
	--build-arg WHERRY_OS="$os" --build-arg WHERRY_ARCH="$arch" --output "type=local,dest=$work/bin" "$repo"
make -C "$repo" image CONTAINER=docker VERSION=e2e WHERRY_VERSION="$WHERRY_VERSION"
previous=$(git -C "$repo" describe --tags --abbrev=0 --match 'kubernetes/v*' HEAD^) ||
	fail "no kubernetes/v* tag before HEAD; fetch the tags"
echo "previous release: $previous"
echo "${previous#kubernetes/}" >"$work/previous-version"
git -C "$repo" archive "$previous" |
	docker build -f examples/kubernetes/Containerfile --build-arg VERSION="${previous#kubernetes/}" \
		--build-arg WHERRY_VERSION="$WHERRY_VERSION" -t wherry-receiver:previous -
docker build -f "$e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$e2e/agent"
