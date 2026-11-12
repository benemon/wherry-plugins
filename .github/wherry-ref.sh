#!/usr/bin/env bash
# Usage: wherry-ref.sh BIN_DIR [IMAGE...]
# For the e2e lanes while the workflows set WHERRY_REF: builds wherry from wherry-src, the checkout of benemon/wherry at
# WHERRY_REF the lane makes, as wherry's release builds it, into BIN_DIR/wherry for this machine, then puts a Linux build
# for each receiver image IMAGE's architecture at /usr/local/bin/wherry in that image, under the same tag. Each must
# report revision WHERRY_REF in wherry version. Needs Go at the version wherry's go.mod names, and Docker for an IMAGE.
set -euo pipefail
bin=$(mkdir -p "$1" && cd "$1" && pwd)
shift
cd "$(dirname "$0")/.."
src=$PWD/wherry-src

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

[[ ${WHERRY_REF:-} =~ ^[0-9a-f]{40}$ ]] || fail "WHERRY_REF is '${WHERRY_REF:-}', not a full commit SHA"
head=$(git -C "$src" rev-parse HEAD) || fail "$src is not a checkout of benemon/wherry"
[ "$head" = "$WHERRY_REF" ] || fail "$src is at $head, not WHERRY_REF $WHERRY_REF"

# build builds wherry into $1 with the environment assignments after it. GOTOOLCHAIN=local fails a wherry that needs a
# newer Go than the lane set up, rather than downloading one.
build() {
	local out=$1
	shift
	(cd "$src" && env CGO_ENABLED=0 GOTOOLCHAIN=local "$@" go build -trimpath -o "$out" ./cmd/wherry)
}

# reports fails unless the wherry version output on standard input, named $1 in errors, has revision WHERRY_REF.
reports() {
	local out
	out=$(cat)
	echo "$1: $out"
	grep -x -F "revision=$WHERRY_REF" <<<"$out" >/dev/null || fail "$1 does not report revision=$WHERRY_REF"
}

build "$bin/wherry"
"$bin/wherry" version | reports "$bin/wherry"

for image in "$@"; do
	ctx=$(mktemp -d)
	build "$ctx/wherry" GOOS=linux GOARCH="$(docker image inspect -f '{{.Architecture}}' "$image")"
	printf 'FROM %s\nCOPY --chown=0:0 --chmod=0755 wherry /usr/local/bin/wherry\n' "$image" | docker build -t "$image" -f - "$ctx"
	rm -rf "$ctx"
	docker run --rm "$image" version | reports "$image"
done
