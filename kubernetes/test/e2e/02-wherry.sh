#!/usr/bin/env bash
# Fetches the wherry release WHERRY_VERSION for this machine into $E2E_WORK/bin, through the receiver image's own
# fetch stage, which verifies the checksums' cosign signature and the archive's SHA-256. Builds the receiver image with
# make image as wherry-receiver:e2e, with WHERRY_VERSION, and the fake agent's image: provisioning pins the receiver
# image's plugin binary.
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
docker build -f "$e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$e2e/agent"
