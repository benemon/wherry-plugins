#!/usr/bin/env bash
# Tests plugin-protocol.sh, for each plugin named as an argument, or every plugin without arguments, on pluginsdk
# fixtures in both forms: serve.go setting Plugins with link.go's handshake ProtocolVersion, and serve.go setting
# VersionedPlugins with link.go's versionedPlugins map. Intersecting sets print result=run with no notice, disjoint
# ones result=skip with a notice naming both, and every unknown or ambiguous form fails with no result line. Then, in a
# scratch repository and offline, a plugin with no tag, a replace of pluginsdk, a workspace, a vendored module and a
# go.sum without the module's sum and a failed download must each fail with no result line. Needs git with the tags, jq,
# go, and pluginsdk v0.8.0-rc.2 in the module cache, as make check's test leaves it.
set -euo pipefail
cd "$(dirname "$0")/.."
script=$PWD/.github/plugin-protocol.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
status=0

failed() {
	echo "FAIL: $*"
	status=1
}

# sdk writes the fixture directory $1: serve.go setting the ServeConfig field $2, Plugins or VersionedPlugins, and
# link.go with the handshake body $3 and, when $4 is set, versionedPlugins returning the map entries $4.
sdk() {
	mkdir -p "$tmp/$1"
	if [ "$2" = Plugins ]; then
		set_line='		Plugins:         plugin.PluginSet{pluginName: &grpcPlugin{}},'
	elif [ "$2" = VersionedPlugins ]; then
		set_line='		VersionedPlugins: versionedPlugins(&grpcPlugin{}),'
	else
		set_line=$2
	fi
	printf 'package pluginsdk\n\nfunc Serve() {\n\tplugin.Serve(&plugin.ServeConfig{\n\t\tHandshakeConfig: handshake,\n%s\n\t\tLogger: logger,\n\t})\n}\n' \
		"$set_line" >"$tmp/$1/serve.go"
	printf 'package pluginsdk\n\nvar handshake = plugin.HandshakeConfig{\n%s\tMagicCookieKey: "WHERRY_PLUGIN",\n}\n' "$3" >"$tmp/$1/link.go"
	if [ -n "${4:-}" ]; then
		printf '\n// versionedPlugins is every protocol.\nfunc versionedPlugins(p *grpcPlugin) map[int]plugin.PluginSet {\n%s\n}\n' "$4" >>"$tmp/$1/link.go"
	fi
}
sdk h10 Plugins $'\tProtocolVersion:  10,\n'
sdk h9 Plugins $'\tProtocolVersion: 9,\n'
sdk v10 VersionedPlugins '' $'\treturn map[int]plugin.PluginSet{10: {pluginName: p}}'
sdk v9-10 VersionedPlugins '' $'\treturn map[int]plugin.PluginSet{9: {pluginName: p}, 10: {pluginName: p}}'
sdk v10-11 VersionedPlugins '' $'\treturn map[int]plugin.PluginSet{\n\t\t11: {pluginName: p},\n\t\t10: {pluginName: p},\n\t}'
sdk v11 VersionedPlugins '' $'\treturn map[int]plugin.PluginSet{11: {pluginName: p}}'
# Unknown or ambiguous forms.
sdk h-const Plugins $'\tProtocolVersion: protocolVersion,\n'
sdk h-none Plugins ''
sdk h-two Plugins $'\tProtocolVersion: 10,\n\tProtocolVersion: 11,\n'
sdk v-const VersionedPlugins '' $'\treturn map[int]plugin.PluginSet{protocolV10: {pluginName: p}}'
sdk v-empty VersionedPlugins '' $'\treturn map[int]plugin.PluginSet{}'
sdk v-built VersionedPlugins '' $'\tm := map[int]plugin.PluginSet{}\n\tm[10] = plugin.PluginSet{pluginName: p}\n\treturn m'
sdk v-nofunc VersionedPlugins ''
sdk both $'\t\tPlugins: plugin.PluginSet{pluginName: &grpcPlugin{}},\n\t\tVersionedPlugins: versionedPlugins(&grpcPlugin{}),' \
	$'\tProtocolVersion: 10,\n' $'\treturn map[int]plugin.PluginSet{10: {pluginName: p}}'
sdk neither '		Logger2: logger,' $'\tProtocolVersion: 10,\n'
mkdir -p "$tmp/no-serve" && cp "$tmp/h10/link.go" "$tmp/no-serve/"
sdk twice Plugins $'\tProtocolVersion: 10,\n' && cp "$tmp/twice/serve.go" "$tmp/twice/serve2.go"

# expect runs the script for the plugin $1 with the fixtures $3 and $4 and checks its result is $2: run or skip, with a
# notice naming the protocols $5 and $6 for skip, or fail, a non-zero exit with no result line.
expect() {
	local code=0 out
	out=$("$script" "$1" "$tmp/$3" "$tmp/$4" 2>&1) || code=$?
	case $2 in
	run | skip)
		if [ "$code" != 0 ] || ! grep -x "result=$2" <<<"$out" >/dev/null; then
			failed "$1 $3 $4: exit $code, want result=$2: $out"
		elif [ "$2" = skip ] && ! grep -E "^::notice .*protocols $5 and the checkout serves plugin protocols $6, none in common" <<<"$out" >/dev/null; then
			failed "$1 $3 $4: no notice naming protocols $5 and $6: $out"
		elif [ "$2" = run ] && grep -F '::notice' <<<"$out" >/dev/null; then
			failed "$1 $3 $4: a notice for intersecting protocols: $out"
		else
			echo "$1 $3 $4: result=$2"
		fi
		;;
	fail)
		if [ "$code" = 0 ] || grep '^result=' <<<"$out" >/dev/null; then
			failed "$1 $3 $4: exit $code, want a failure with no result: $out"
		else
			echo "$1 $3 $4: exit $code, $(grep -m 1 '^FAIL' <<<"$out")"
		fi
		;;
	esac
}

plugins=("$@")
[ $# -gt 0 ] || plugins=(nomad kubernetes systemd)
for plugin in "${plugins[@]}"; do
	expect "$plugin" run h10 h10
	expect "$plugin" run h10 v10
	expect "$plugin" run v9-10 v10-11
	expect "$plugin" skip h9 v10 9 10
	expect "$plugin" skip v9-10 v11 '9 10' 11
	for bad in h-const h-none h-two v-const v-empty v-built v-nofunc both neither no-serve twice; do
		expect "$plugin" fail h10 "$bad"
		expect "$plugin" fail "$bad" h10
	done
done

# A scratch repository with the script, a plugin p tagged p/v0.0.1, and a checkout one commit later. GOPROXY=off keeps
# go offline; the checks below fail before any download, or because of it.
repo=$tmp/repo
mkdir -p "$repo/.github" "$repo/p"
cp "$script" "$repo/.github/"
printf 'module example.com/p\n\ngo 1.24\n\nrequire github.com/benemon/wherry/pluginsdk v0.8.0-rc.2\n' >"$repo/p/go.mod"
grep '^github.com/benemon/wherry/pluginsdk v0.8.0-rc.2 ' systemd/go.sum >"$repo/p/go.sum"
git -C "$repo" init -q
git -C "$repo" -c user.name=t -c user.email=t@invalid commit -q --allow-empty -m first
git -C "$repo" add -A
git -C "$repo" -c user.name=t -c user.email=t@invalid commit -q -m p
# scratch runs the scratch repository's script for the plugin $1 after the shell commands $2 and checks it fails with
# no result line and an error matching $3.
scratch() {
	local code=0 out
	out=$(cd "$repo" && eval "$2" && GOPROXY=off GOFLAGS='' .github/plugin-protocol.sh "$1" 2>&1) || code=$?
	if [ "$code" = 0 ] || grep '^result=' <<<"$out" >/dev/null || ! grep -E "$3" <<<"$out" >/dev/null; then
		failed "scratch $1 after '$2': exit $code, want a failure matching '$3': $out"
	else
		echo "scratch $1 after '$2': exit $code, $(grep -m 1 '^FAIL' <<<"$out")"
	fi
}
scratch p : 'no p/v\* tag'
git -C "$repo" tag p/v0.0.1 HEAD
git -C "$repo" -c user.name=t -c user.email=t@invalid commit -q --allow-empty -m later
scratch no-such-plugin : 'no no-such-plugin/v\* tag'
scratch p 'printf "replace github.com/benemon/wherry/pluginsdk => ../sdk\n" >>p/go.mod' 'replaces github.com/benemon/wherry/pluginsdk'
scratch p 'git checkout -q -- p/go.mod && printf "replace (\n\tgithub.com/benemon/wherry/pluginsdk v0.8.0-rc.2 => github.com/other/sdk v1.0.0\n)\n" >>p/go.mod' \
	'replaces github.com/benemon/wherry/pluginsdk'
scratch p 'git checkout -q -- p/go.mod && printf "go 1.24\n\nuse ./p\n" >go.work' 'has go.work'
scratch p 'rm go.work && mkdir -p p/vendor && : >p/vendor/modules.txt' 'has p/vendor/modules.txt'
scratch p 'rm -r p/vendor && : >p/go.sum' "but the checkout's p/go.sum records ''"
scratch p 'git checkout -q -- p/go.sum && sed -i "s/v0.8.0-rc.2/v0.0.1-nosuch/" p/go.mod' 'go mod download .*v0.0.1-nosuch failed'
exit "$status"
