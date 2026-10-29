#!/bin/sh
# Installs the release build's tools into the directory $1, each pinned by version and SHA-256: goreleaser, syft, which
# writes the SBOMs, and cosign, which signs checksums.txt. release.yml and ci.yml's release-build job run it.
set -eu
GORELEASER_VERSION=2.19.0
GORELEASER_SHA256=ebc3194ad1d1b1787614405285a0039665cd4c5bd3305215a1b201600f90630b
SYFT_VERSION=1.55.0
SYFT_SHA256=bb65249542db5fedeeb4d3f9b018b75e7264dbfbbb10486f044581a92f90490d
COSIGN_VERSION=3.1.4
COSIGN_SHA256=424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa

bin=$1
tmp=$(mktemp -d)
mkdir -p "$bin"
# fetch FILE REPO TAG ASSET SHA256 downloads the release asset into $tmp/FILE and checks its SHA-256.
fetch() {
	curl -fsSL -o "$tmp/$1" "https://github.com/$2/releases/download/$3/$4"
	echo "$5  $tmp/$1" | sha256sum -c -
}
fetch goreleaser.tar.gz goreleaser/goreleaser "v$GORELEASER_VERSION" goreleaser_Linux_x86_64.tar.gz "$GORELEASER_SHA256"
tar -xzf "$tmp/goreleaser.tar.gz" -C "$bin" goreleaser
fetch syft.tar.gz anchore/syft "v$SYFT_VERSION" "syft_${SYFT_VERSION}_linux_amd64.tar.gz" "$SYFT_SHA256"
tar -xzf "$tmp/syft.tar.gz" -C "$bin" syft
fetch cosign sigstore/cosign "v$COSIGN_VERSION" cosign-linux-amd64 "$COSIGN_SHA256"
install -m 0755 "$tmp/cosign" "$bin/cosign"
rm -rf "$tmp"
