#!/usr/bin/env bash
# Fetches the wherry release WHERRY_VERSION for this machine into $E2E_WORK/bin, through the receiver image's own
# fetch stage, which verifies the checksums' cosign signature and the archive's SHA-256. Builds the receiver image with
# make image as wherry-receiver:e2e with plugin version e2e, from the latest kubernetes/v* tag before HEAD as
# wherry-receiver:previous with that tag's version, the tag less its kubernetes/ prefix, which it keeps in
# $E2E_WORK/previous-version, both with WHERRY_VERSION, and the fake agent's image: provisioning pins the plugin binary
# of the image the receiver runs. First it runs the plugin protocol check, which finds the tag and keeps its result for
# the later steps. With no kubernetes/v* tag before HEAD, or any other failure of the check, it fails. With WHERRY_REF
# set, .github/wherry-ref.sh builds the CLI from that commit instead, and puts it in both receiver images.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "unsupported machine $(uname -m)" >&2 && exit 1 ;;
esac
if [ -z "${WHERRY_REF:-}" ]; then
	docker build -f "$repo/examples/kubernetes/Containerfile" --target wherry --build-arg WHERRY_VERSION="$WHERRY_VERSION" \
		--build-arg WHERRY_OS="$os" --build-arg WHERRY_ARCH="$arch" --output "type=local,dest=$work/bin" "$repo"
fi
protocol_check
previous=$(checked previous)
previous_version=${previous#kubernetes/}
echo "previous release: $previous, plugin version $previous_version"
echo "$previous_version" >"$work/previous-version"
make -C "$repo" image CONTAINER=docker VERSION=e2e WHERRY_VERSION="$WHERRY_VERSION"
git -C "$repo" archive "$previous" |
	docker build -f examples/kubernetes/Containerfile --build-arg VERSION="$previous_version" \
		--build-arg WHERRY_VERSION="$WHERRY_VERSION" -t wherry-receiver:previous -
if [ -n "${WHERRY_REF:-}" ]; then
	"$repo/.github/wherry-ref.sh" "$work/bin" wherry-receiver:e2e wherry-receiver:previous
fi
docker build -f "$e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$e2e/agent"
