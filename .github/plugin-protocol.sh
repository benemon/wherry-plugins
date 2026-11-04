#!/usr/bin/env bash
# Usage: plugin-protocol.sh PLUGIN [PREVIOUS_SDK_DIR CURRENT_SDK_DIR]
# Compares the plugin protocols the plugin's previous release serves with those this checkout serves. The previous
# release is the latest PLUGIN/v* tag before HEAD, as the e2e lanes build it. Each side serves the protocols of the
# pluginsdk module at the version its PLUGIN/go.mod requires, which go mod download fetches and verifies against the
# checksum database and the side's go.sum:
# - serve.go's ServeConfig sets Plugins, with HandshakeConfig handshake: link.go's handshake ProtocolVersion;
# - it sets VersionedPlugins to versionedPlugins(...): the keys of the map link.go's versionedPlugins returns.
# The sides are compatible when their sets intersect. The two directories given as arguments take the place of the
# downloaded modules, to test the parsing and the comparison.
#
# It prints each side's protocols, then these lines, and exits 0:
#   previous=<tag>
#   previous_protocols=<protocols, ascending, space-separated>
#   current_protocols=<protocols>
#   result=run    when the sets intersect;
#   result=skip   when they do not, after a GitHub Actions notice naming both: the lanes then skip the previous-release
#                 roll, since across a protocol break the release notes say to upgrade the receiver and its plugins
#                 together, or to reset.
# Every other outcome exits non-zero and prints no result line: no tag, a fetch or checksum error, a replace or
# workspace override of pluginsdk, a vendored module, or a serve.go or link.go in a form it does not know. Needs git
# with the tags, jq, and go unless both directories are given.
set -euo pipefail
module=github.com/benemon/wherry/pluginsdk

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# sdk_version prints the pluginsdk version the go.mod file $1 requires, named $2 in errors. It fails on a replace of
# pluginsdk, with or without a version, since the plugin then builds against something other than that version.
sdk_version() {
	local json version
	json=$(go mod edit -json "$1") || fail "$2 does not parse"
	if jq -e --arg m "$module" 'any(.Replace[]?; .Old.Path == $m)' <<<"$json" >/dev/null; then
		fail "$2 replaces $module; the check compares a required version only"
	fi
	version=$(jq -r --arg m "$module" '[.Require[]? | select(.Path == $m) | .Version] |
		if length == 1 then .[0] else "" end' <<<"$json")
	[ -n "$version" ] || fail "$2 does not require $module exactly once"
	echo "$version"
}

# sdk_dir prints the directory of the pluginsdk module at the version $1, downloaded by go mod download, which verifies
# it against the checksum database. Its Sum must also be the h1 line the go.sum file $2, named $3 in errors, records.
sdk_dir() {
	local json dir sum recorded
	[ "$(go env GOSUMDB)" != off ] || fail "GOSUMDB is off, so go mod download would not verify $module@$1"
	json=$(cd "$tmp" && env GOWORK=off GOFLAGS=-modcacherw GONOSUMDB= GOPRIVATE= GOINSECURE= \
		go mod download -json "$module@$1") || fail "go mod download $module@$1 failed: $json"
	jq -e 'has("Error") | not' <<<"$json" >/dev/null || fail "go mod download $module@$1: $(jq -r .Error <<<"$json")"
	dir=$(jq -r '.Dir // empty' <<<"$json")
	sum=$(jq -r '.Sum // empty' <<<"$json")
	[ -n "$dir" ] && [ -f "$dir/link.go" ] && [ -f "$dir/serve.go" ] || fail "$module@$1 has no link.go and serve.go"
	recorded=$(awk -v m="$module" -v v="$1" '$1 == m && $2 == v { print $3 }' "$2")
	[ -n "$sum" ] && [ "$sum" = "$recorded" ] ||
		fail "$module@$1 downloaded with sum '$sum', but $3 records '$recorded'"
	echo "$dir"
}

# no_override fails when one of the paths $2... exists in the tree $1, a tag or the checkout, or, for the checkout, when
# go would use a workspace: a go.work or a vendored module would build the plugin against another pluginsdk.
no_override() {
	local where=$1 path
	shift
	for path in "$@"; do
		if tree_has "$where" "$path"; then fail "$where has $path, which overrides the required $module"; fi
	done
	if [ "$where" = checkout ]; then
		path=$(cd "$plugin" && go env GOWORK)
		[ -z "$path" ] || [ "$path" = off ] || fail "go uses the workspace $path for $plugin; set GOWORK=off"
	fi
}
tree_has() {
	if [ "$1" = checkout ]; then [ -e "$2" ]; else [ -n "$(git ls-tree --name-only "$1" -- "$2")" ]; fi
}

# protocols prints the plugin protocols the pluginsdk directory $1 serves, ascending and space-separated, and fails on
# any form it does not know.
protocols() {
	local dir=$1 sources serve field found
	sources=$(find "$dir" -maxdepth 1 -name '*.go' ! -name '*_test.go' | sort)
	[ -n "$sources" ] || fail "$dir holds no Go source"
	# defined prints how many of the module's own files' lines match the pattern $1, and fails unless it is one, in $2.
	defined() {
		local count where
		# shellcheck disable=SC2086 # sources holds paths without spaces from the module cache or a fixture
		count=$(cat $sources | grep -c -E "$1" || true)
		[ "$count" = 1 ] || fail "$dir: $count lines match '$1', want 1"
		# shellcheck disable=SC2086
		where=$(grep -l -E "$1" $sources)
		[ "$(basename "$where")" = "$2" ] || fail "$dir: '$1' is in $(basename "$where"), not $2"
	}
	defined 'plugin\.ServeConfig\{' serve.go
	# ServeConfig's literal: from its line to the first line that closes it at the call's indentation.
	serve=$(awk '/plugin\.ServeConfig\{/ { body = 1; next } body && /^[[:space:]]*\}\)/ { exit } body' "$dir/serve.go")
	field=$(grep -o -E '^[[:space:]]*(Plugins|VersionedPlugins):' <<<"$serve" | tr -d ' \t:' || true)
	case $field in
	Plugins)
		grep -E '^[[:space:]]*HandshakeConfig:[[:space:]]*handshake,$' <<<"$serve" >/dev/null ||
			fail "$dir/serve.go: ServeConfig sets Plugins but not HandshakeConfig: handshake"
		defined '^var handshake = plugin\.HandshakeConfig\{$' link.go
		found=$(awk '/^var handshake = plugin\.HandshakeConfig\{$/ { body = 1; next } body && /^\}$/ { exit } body' \
			"$dir/link.go" | grep 'ProtocolVersion' || true)
		[[ $found =~ ^[[:space:]]*ProtocolVersion:[[:space:]]*([1-9][0-9]*),$ ]] ||
			fail "$dir/link.go: handshake's ProtocolVersion is not one decimal literal: '$found'"
		echo "${BASH_REMATCH[1]}"
		;;
	VersionedPlugins)
		grep -E '^[[:space:]]*VersionedPlugins:[[:space:]]*versionedPlugins\(.*\),$' <<<"$serve" >/dev/null ||
			fail "$dir/serve.go: ServeConfig's VersionedPlugins is not versionedPlugins(...)"
		defined '^func versionedPlugins\(' link.go
		# The function's body must be a single return of a map literal whose keys are decimal literals.
		awk '/^func versionedPlugins\(/ { body = 1; next } body && /^\}$/ { exit } body && !/^[[:space:]]*\/\// { print }' \
			"$dir/link.go" | tr '\n' ' ' | awk -v dir="$dir" '
			{
				s = $0
				if (!sub(/^[[:space:]]*return[[:space:]]+map\[int\]plugin\.PluginSet\{/, "", s)) bad("no return of map[int]plugin.PluginSet{...}")
				depth = 0; key = ""; keys = 0
				for (i = 1; i <= length(s); i++) {
					c = substr(s, i, 1)
					if (c == "{") { depth++; continue }
					if (c == "}") { if (depth == 0) { rest = substr(s, i + 1); break } depth--; continue }
					if (depth > 0) continue
					if (c == ":" && !invalue) {
						gsub(/[[:space:]]/, "", key)
						if (key !~ /^[1-9][0-9]*$/) bad("a key is not a decimal literal: " key)
						print key; keys++; key = ""; invalue = 1; continue
					}
					if (c == ",") { invalue = 0; continue }
					if (!invalue) key = key c
				}
				gsub(/[[:space:]]/, "", rest); gsub(/[[:space:]]/, "", key)
				if (rest != "" || key != "") bad("unexpected text after the map: " rest key)
				if (keys == 0) bad("the map has no keys")
			}
			function bad(why) { print "FAIL: " dir "/link.go: versionedPlugins: " why > "/dev/stderr"; exit 1 }'
		;;
	*) fail "$dir/serve.go: ServeConfig sets neither Plugins nor VersionedPlugins alone: '${field//$'\n'/ }'" ;;
	esac | sort -n -u | tr '\n' ' ' | sed 's/ $//'
	# The pipeline's status is the last command's; a failure above must still fail.
	[ "${PIPESTATUS[0]}" = 0 ] || exit 1
}

[ $# = 1 ] || [ $# = 3 ] || fail "usage: plugin-protocol.sh PLUGIN [PREVIOUS_SDK_DIR CURRENT_SDK_DIR]"
plugin=$1
cd "$(dirname "$0")/.."
previous=$(git describe --tags --abbrev=0 --match "$plugin/v*" HEAD^) ||
	fail "no $plugin/v* tag before HEAD; fetch the tags, as fetch-depth: 0 does"

if [ $# = 3 ]; then
	previous_dir=$2 current_dir=$3
	previous_sdk="(the directory $2)" current_sdk="(the directory $3)"
else
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	no_override "$previous" go.work "$plugin/go.work" "$plugin/vendor/modules.txt"
	no_override checkout go.work "$plugin/go.work" "$plugin/vendor/modules.txt"
	git show "$previous:$plugin/go.mod" >"$tmp/previous.mod" || fail "$previous has no $plugin/go.mod"
	git show "$previous:$plugin/go.sum" >"$tmp/previous.sum" || fail "$previous has no $plugin/go.sum"
	previous_sdk=$(sdk_version "$tmp/previous.mod" "$previous:$plugin/go.mod")
	current_sdk=$(sdk_version "$plugin/go.mod" "the checkout's $plugin/go.mod")
	previous_dir=$(sdk_dir "$previous_sdk" "$tmp/previous.sum" "$previous:$plugin/go.sum")
	current_dir=$(sdk_dir "$current_sdk" "$plugin/go.sum" "the checkout's $plugin/go.sum")
fi
previous_protocols=$(protocols "$previous_dir")
current_protocols=$(protocols "$current_dir")
[ -n "$previous_protocols" ] && [ -n "$current_protocols" ] || fail "a side serves no protocol"
common=$(comm -12 <(tr ' ' '\n' <<<"$previous_protocols" | sort) <(tr ' ' '\n' <<<"$current_protocols" | sort) | sort -n | tr '\n' ' ')

echo "$previous serves plugin protocols $previous_protocols, pluginsdk $previous_sdk"
echo "the checkout serves plugin protocols $current_protocols, pluginsdk $current_sdk"
echo "previous=$previous"
echo "previous_protocols=$previous_protocols"
echo "current_protocols=$current_protocols"
if [ -z "$common" ]; then
	echo "::notice title=$plugin previous-release roll skipped::$previous serves plugin protocols $previous_protocols and the checkout serves plugin protocols $current_protocols, none in common, so the lane skips the roll between them; upgrade the receiver and its plugins together, or reset."
	echo "result=skip"
else
	echo "result=run"
fi
