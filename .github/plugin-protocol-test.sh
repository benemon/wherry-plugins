#!/usr/bin/env bash
# Tests plugin-protocol.sh's comparison on link.go fixtures in both of pluginsdk's forms, for each plugin named as an
# argument, or every plugin without arguments: the same protocols pass without a notice, different ones exit 2 with a
# notice naming both, and a plugin with no release tag fails. Needs git with the tags.
set -euo pipefail
cd "$(dirname "$0")/.."
script=.github/plugin-protocol.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
status=0

failed() {
	echo "FAIL: $*"
	status=1
}

handshake() { printf 'var handshake = plugin.HandshakeConfig{\n\tProtocolVersion:  %s,\n\tMagicCookieKey: "WHERRY_PLUGIN",\n}\n' "$1" >"$tmp/$2"; }
versioned() { printf 'func versionedPlugins(p *grpcPlugin) map[int]plugin.PluginSet {\n\treturn map[int]plugin.PluginSet{%s}\n}\n' "$1" >"$tmp/$2"; }
handshake 10 h10.go
handshake 9 h9.go
versioned '10: {pluginName: p}' v10.go
versioned '9: {pluginName: p}, 10: {pluginName: p}' v9-10.go
printf 'func versionedPlugins(p *grpcPlugin) map[int]plugin.PluginSet {\n\treturn map[int]plugin.PluginSet{\n\t\t10: {pluginName: p},\n\t\t9:  {pluginName: p},\n\t}\n}\n' >"$tmp/v10-9.go"

# expect runs the script for the plugin $1 with the fixtures $3 and $4, and checks its exit code is $2 and, for 2, that
# its notice names the protocols $5 and $6.
expect() {
	local code=0 out
	out=$("$script" "$1" "$tmp/$3" "$tmp/$4" 2>&1) || code=$?
	if [ "$code" != "$2" ]; then
		failed "$1 $3 $4: exit $code, want $2: $out"
	elif [ "$2" = 2 ] && ! grep -E "^::notice .*protocol $5 and the checkout serves plugin protocol $6," <<<"$out" >/dev/null; then
		failed "$1 $3 $4: no notice naming protocols $5 and $6: $out"
	elif [ "$2" = 0 ] && grep -F '::notice' <<<"$out" >/dev/null; then
		failed "$1 $3 $4: a notice for the same protocol: $out"
	else
		echo "$1 $3 $4: exit $code"
	fi
}

plugins=("$@")
[ $# -gt 0 ] || plugins=(nomad kubernetes systemd)
for plugin in "${plugins[@]}"; do
	expect "$plugin" 0 h10.go h10.go
	expect "$plugin" 0 h10.go v10.go
	expect "$plugin" 0 v9-10.go v10-9.go
	expect "$plugin" 2 h9.go v10.go 9 10
	expect "$plugin" 2 v10.go v9-10.go 10 '9 10'
done
code=0
"$script" no-such-plugin "$tmp/h10.go" "$tmp/h10.go" >/dev/null 2>&1 || code=$?
[ "$code" = 1 ] || failed "a plugin with no release tag exited $code, want 1"
exit "$status"
