#!/usr/bin/env bash
# Usage: wherry-ref-check.sh PLUGIN...
# Checks WHERRY_REF in .github/workflows/ci.yml and e2e.yml: the same in both, and '' or a full commit SHA in single
# quotes. When it is set, it must be the commit of the pluginsdk tag each PLUGIN/go.mod requires, as go mod download
# reports it in Origin: the tag keeps the commit fetchable once wherry squash-merges its pull request, and wherry and
# the plugins then build from the same pluginsdk. Needs go and jq when it is set.
set -euo pipefail
module=github.com/benemon/wherry/pluginsdk
cd "$(dirname "$0")/.."

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

ref() { sed -n 's/^  WHERRY_REF: //p' ".github/workflows/$1"; }
ref=$(ref ci.yml)
[ "$ref" = "$(ref e2e.yml)" ] || fail "ci.yml sets WHERRY_REF $ref and e2e.yml $(ref e2e.yml); change both together"
[[ $ref =~ ^\'([0-9a-f]{40})?\'$ ]] || fail "WHERRY_REF is $ref, not '' or a full commit SHA in single quotes"
want=${BASH_REMATCH[1]}
if [ -z "$want" ]; then
	echo "WHERRY_REF is empty"
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for plugin in "$@"; do
	version=$(go mod edit -json "$plugin/go.mod" | jq -r --arg m "$module" '[.Require[]? | select(.Path == $m) | .Version] |
		if length == 1 then .[0] else "" end')
	[ -n "$version" ] || fail "$plugin/go.mod does not require $module exactly once"
	json=$(cd "$tmp" && env GOWORK=off GOFLAGS=-modcacherw go mod download -json "$module@$version") ||
		fail "go mod download $module@$version failed: $json"
	origin=$(jq -r '"\(.Origin.Ref // "") \(.Origin.Hash // "")"' <<<"$json")
	[ "$origin" = "refs/tags/pluginsdk/$version $want" ] ||
		fail "$plugin requires $module $version, whose origin is '$origin', not the tag pluginsdk/$version at WHERRY_REF $want"
	echo "$plugin: $module $version is the tag pluginsdk/$version at WHERRY_REF $want"
done
