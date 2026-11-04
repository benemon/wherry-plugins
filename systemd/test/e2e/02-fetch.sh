#!/usr/bin/env bash
# Fetches and verifies the binaries the install and the upgrade install, each in an empty directory of its own:
# - wherry of the release WHERRY_VERSION, with step 1's commands of systemd/README.md's Run the receiver as a systemd
#   service, into $E2E_WORK/wherry, and, as the harness's CLI, into $E2E_WORK/bin;
# - the systemd plugin of the previous release, the latest systemd/v* tag before HEAD, with the commands of Install
#   plugins that step 1 names, into $E2E_WORK/previous, keeping its version in $E2E_WORK/previous-version;
# - the plugin built from the checkout with version e2e, as step 1 allows and both Containerfiles build, into
#   $E2E_WORK/current.
# Needs cosign and Go. With no systemd/v* tag before HEAD it fails, as the upgrade has no release to start from.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
arch=$(arch)

# Step 1: wherry.
mkdir "$work/wherry"
(
	cd "$work/wherry"
	base=https://github.com/benemon/wherry/releases/download/$WHERRY_VERSION
	archive=wherry_${WHERRY_VERSION}_linux_$arch.tar.gz
	curl -fsSL --remote-name-all "$base/$archive" "$base/checksums.txt" "$base/checksums.txt.sigstore.json"
	cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
		--certificate-identity-regexp '^https://github\.com/benemon/wherry/\.github/workflows/release\.yml@refs/tags/v' \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com
	sha256sum --ignore-missing -c checksums.txt
	tar -xzf "$archive" --strip-components=1 "${archive%.tar.gz}/wherry"
)
install -m 0755 "$work/wherry/wherry" "$work/bin/wherry"
wherry version

# Step 1, Install plugins: the previous release's plugin. Step 2 checks its pin against these checksums.
previous=$(git -C "$repo" describe --tags --abbrev=0 --match 'systemd/v*' HEAD^) ||
	fail "no systemd/v* tag before HEAD; fetch the tags"
echo "previous release: $previous"
echo "${previous#systemd/}" >"$work/previous-version"
mkdir "$work/previous"
(
	cd "$work/previous"
	plugin=systemd version=${previous#systemd/} os=linux
	base=https://github.com/benemon/wherry-plugins/releases/download/$plugin/$version
	archive=wherry-plugin-${plugin}_${version}_${os}_${arch}.tar.gz
	curl -fsSL --remote-name-all "$base/$archive" "$base/checksums.txt" "$base/checksums.txt.sigstore.json"
	cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
		--certificate-identity-regexp "^https://github\.com/benemon/wherry-plugins/\.github/workflows/release\.yml@refs/tags/$plugin/v" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com
	grep "  $archive\$" checksums.txt | sha256sum -c -
	tar -xzf "$archive" --strip-components=1 "${archive%.tar.gz}/wherry-plugin-$plugin"
)

# Step 1, built from the checkout instead, with the version both Containerfiles give the checkout's build.
mkdir "$work/current"
(cd "$repo/systemd" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-X main.version=e2e" \
	-o "$work/current/wherry-plugin-systemd" .)
