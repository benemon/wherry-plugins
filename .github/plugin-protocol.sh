#!/usr/bin/env bash
# Usage: plugin-protocol.sh PLUGIN [PREVIOUS_LINK_GO CURRENT_LINK_GO]
# Compares the plugin protocol the plugin's previous release serves with the one this checkout serves. The previous
# release is the latest PLUGIN/v* tag before HEAD, as the e2e lanes build it; the step fails when there is none. Each
# side serves the protocols of pluginsdk/link.go at the pluginsdk version its PLUGIN/go.mod requires: the keys of
# versionedPlugins, or, before that function, handshake's ProtocolVersion. The two files given as arguments take the
# place of the downloaded link.go files, to test the comparison.
# Exits 0 when both serve the same protocols, and 2, with a GitHub Actions notice naming both, when they differ: the
# lanes then skip the previous-release roll, since across a protocol break the release notes say to upgrade the
# receiver and its plugins together, or to reset. Exits 1 on any error. Needs git with the tags, and go unless both
# files are given.
set -euo pipefail
module=github.com/benemon/wherry/pluginsdk

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# sdk_version prints the pluginsdk version the go.mod on standard input requires, named $1 in errors.
sdk_version() {
	local gomod version
	gomod=$(cat)
	if awk -v m="$module" '$1 == m && $2 == "=>" || $1 == "replace" && $2 == m' <<<"$gomod" | grep . >/dev/null; then
		fail "$1 replaces $module; compare a required version only"
	fi
	version=$(awk -v m="$module" '$1 == "require" && $2 == m { print $3; exit } $1 == m && $2 ~ /^v/ { print $2; exit }' <<<"$gomod")
	[ -n "$version" ] || fail "$1 requires no $module"
	echo "$version"
}

# link_go prints the path of link.go in the pluginsdk module at the version $1, downloading and verifying the module
# against go.sum's checksum database.
link_go() {
	local dir
	dir=$(cd "$tmp" && go mod download -json "$module@$1" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
	[ -n "$dir" ] && [ -f "$dir/link.go" ] || fail "$module@$1 has no link.go"
	echo "$dir/link.go"
}

# protocols prints the plugin protocols the link.go $1 serves, ascending and space-separated.
protocols() {
	local found
	if grep -E '^func versionedPlugins\(' "$1" >/dev/null; then
		# The map's keys, on one line or several, are the integers before a colon after its opening brace.
		found=$(awk '/^func versionedPlugins\(/ { body = 1; next } body && /^}/ { exit } body' "$1" | tr '\n' ' ' |
			sed 's/^.*PluginSet{//' | grep -oE '(^|[{,[:space:]])[0-9]+:' | tr -dc '0-9\n' || true)
	else
		found=$(sed -n 's/^[[:space:]]*ProtocolVersion:[[:space:]]*\([0-9][0-9]*\),.*$/\1/p' "$1")
	fi
	found=$(sort -n -u <<<"$found" | grep . | tr '\n' ' ' || true)
	[ -n "$found" ] || fail "$1 names no plugin protocol in versionedPlugins or handshake"
	echo "${found% }"
}

[ $# = 1 ] || [ $# = 3 ] || fail "usage: plugin-protocol.sh PLUGIN [PREVIOUS_LINK_GO CURRENT_LINK_GO]"
plugin=$1
cd "$(dirname "$0")/.."
previous=$(git describe --tags --abbrev=0 --match "$plugin/v*" HEAD^ 2>/dev/null) ||
	fail "no $plugin/v* tag before HEAD; fetch the tags, as fetch-depth: 0 does"

if [ $# = 3 ]; then
	previous_link=$2 current_link=$3
	previous_sdk="(from $2)" current_sdk="(from $3)"
else
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	previous_sdk=$(git show "$previous:$plugin/go.mod" | sdk_version "$previous:$plugin/go.mod")
	current_sdk=$(sdk_version "the checkout's $plugin/go.mod" <"$plugin/go.mod")
	previous_link=$(link_go "$previous_sdk")
	current_link=$(link_go "$current_sdk")
fi
previous_protocol=$(protocols "$previous_link")
current_protocol=$(protocols "$current_link")

echo "$previous serves plugin protocol $previous_protocol, pluginsdk $previous_sdk"
echo "the checkout serves plugin protocol $current_protocol, pluginsdk $current_sdk"
if [ "$previous_protocol" != "$current_protocol" ]; then
	echo "::notice title=$plugin previous-release roll skipped::$previous serves plugin protocol $previous_protocol and the checkout serves plugin protocol $current_protocol, so the lane skips the roll between them; upgrade the receiver and its plugins together, or reset."
	exit 2
fi
