# shellcheck shell=bash disable=SC2034
# Sourced by each e2e step. The steps share their state in E2E_WORK, outside the checkout. They run as a user with root
# through sudo: the README's host commands run as root, and the operator and host commands as the user.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
e2e=$repo/systemd/test/e2e
work=${E2E_WORK:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wherry-systemd-e2e}
# The operator's working directory, and the host's own paths under $E2E_WORK/host. $E2E_WORK/bin holds the wherry
# CLI that provisions and lists the host's receivers.
operator=$work/operator
libexec=/usr/local/libexec/wherry
export PATH=$work/bin:$PATH XDG_CONFIG_HOME=$work/host/config XDG_STATE_HOME=$work/host/state

# mask hides a secret in the GitHub Actions log; the steps never print one.
mask() { if [ "${GITHUB_ACTIONS:-}" = true ]; then echo "::add-mask::$1"; fi; }

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# wait_for runs a command every 5 seconds until it succeeds, for at most the given seconds.
wait_for() {
	local end=$((SECONDS + $1))
	shift
	until "$@"; do
		if ((SECONDS >= end)); then
			echo "timed out: $*" >&2
			return 1
		fi
		sleep 5
	done
}

host() { wherry host "$@"; }

# arch prints the release architecture of this machine.
arch() {
	case $(uname -m) in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) fail "unsupported machine $(uname -m)" ;;
	esac
}

# previous_version prints the plugin version of the previous release, which 02-fetch.sh downloads.
previous_version() { cat "$work/previous-version"; }

# protocol_check runs .github/plugin-protocol.sh for the systemd plugin and keeps its result lines in
# $work/protocol.env: the previous release's tag, the protocols each side serves, and result=run or result=skip. Any
# failure of the check fails the step; a skip is only ever the check's result=skip, never an error.
protocol_check() {
	"$repo/.github/plugin-protocol.sh" systemd | tee "$work/protocol.out"
	grep -E '^(previous|previous_protocols|current_protocols|result)=' "$work/protocol.out" >"$work/protocol.env"
}

# checked prints the value of the key $1 that protocol_check kept, and fails the step when it has none.
checked() {
	local value
	value=$(sed -n "s/^$1=//p" "$work/protocol.env" 2>/dev/null || true)
	[ -n "$value" ] || fail "02-fetch.sh's protocol check kept no $1"
	echo "$value"
}

# protocol prints the plugin protocol the receiver runs with the side $1, previous or current: the newest it serves.
protocol() {
	local protocols
	protocols=$(checked "$1_protocols") || exit 1
	tr ' ' '\n' <<<"$protocols" | sort -n | tail -n 1
}

# same_protocol succeeds when protocol_check found protocols both sides serve. When it found none it repeats the check's
# notice and fails, and the lane skips its roll to and from the previous release. Any other value fails the step.
same_protocol() {
	local result
	result=$(checked result) || exit 1
	case $result in
	run) return 0 ;;
	skip)
		grep '^::notice ' "$work/protocol.out" || true
		echo "SKIP: $(basename "$0"): the previous release and the checkout serve no plugin protocol in common"
		return 1
		;;
	*) fail "the plugin protocol check's result is '$result'" ;;
	esac
}

# set_pin sets plugin_sha256.systemd in the receiver's file to the SHA-256 of the installed plugin of the version $1, as
# step 2 obtains it, and applies the file, as step 3 and the Upgrade's step 2 do. pinned then checks the record.
set_pin() {
	local sha256 file=$operator/receivers/systemd-1.yaml
	sha256=$(sha256sum "$libexec/$1/plugins/wherry-plugin-systemd" | cut -d' ' -f1)
	sed -i '/^plugin_sha256:$/,$d' "$file"
	printf 'plugin_sha256:\n  systemd: %s\n' "$sha256" >>"$file"
	wherry operator receiver apply systemd-1 --dir "$operator"
	pinned "$sha256"
}

# pinned fails the step unless the receiver systemd-1's record, as receiver show prints it, pins the SHA-256 $1.
pinned() {
	local recorded
	recorded=$(wherry operator receiver show systemd-1 --dir "$operator" |
		awk '/^plugin_sha256:/ { pins = 1; next } /^[^ ]/ { pins = 0 } pins && $1 == "systemd:" { print $2 }')
	[ "$recorded" = "$1" ] || fail "the receiver's record pins '$recorded', not $1"
}

# invocation prints the receiver unit's current invocation ID, empty before its first start.
invocation() { systemctl show -p InvocationID --value wherry-receiver; }

# boundary records a deliberate restart boundary in $work/boundary, immediately before the step starts or restarts the
# receiver: the unit's invocation then, and the cursor of the journal's newest entry. evidence reads the receiver's
# journal after that cursor alone, so a refusal before the boundary, which the README's order allows between the new
# pin and the repoint, is not the step's to judge.
boundary() {
	local before cursor
	before=$(invocation) || fail "systemctl show failed for wherry-receiver"
	cursor=$(sudo journalctl -q -n 1 -o json --no-pager | jq -r '.__CURSOR // empty') ||
		fail "journalctl could not print the journal's newest entry"
	[ -n "$cursor" ] || fail "the journal's newest entry has no cursor"
	printf '%s\n%s\n' "$before" "$cursor" >"$work/boundary"
	echo "boundary: invocation '$before', cursor $cursor"
}

# post_boundary prints every entry of the receiver unit's journal after the boundary's cursor, one per line as its
# invocation ID, a tab and its message: the receiver's own lines carry _SYSTEMD_INVOCATION_ID, and systemd's lines
# about the unit INVOCATION_ID, so a start that failed before the receiver logged anything still names its invocation.
# It fails when journalctl or jq does.
post_boundary() {
	local cursor
	cursor=$(sed -n 2p "$work/boundary")
	[ -n "$cursor" ] || fail "the step recorded no boundary"
	sudo journalctl -u wherry-receiver --after-cursor="$cursor" -o json --no-pager |
		jq -r '[._SYSTEMD_INVOCATION_ID // .INVOCATION_ID // "",
			(.MESSAGE // "" | if type == "array" then implode else tostring end)] | @tsv'
}

# evidence checks every invocation of the receiver after the boundary, failed starts and restarts included, as step 6
# and the Upgrade's step 4 check the identity line. It fails the step when the journal cannot be read, when any of
# them logged a line ending "and the pin is <pin>", or when any logged an identity line other than
# "plugin systemd: version $1 protocol $2". It succeeds, setting fresh, once the unit's current invocation is non-empty,
# is not the one the boundary saw, and has logged that identity line; until then it fails, for wait_for to retry.
evidence() {
	local want="plugin systemd: version $1 protocol $2" before entries target id message line seen=
	before=$(sed -n 1p "$work/boundary")
	entries=$(post_boundary) || fail "could not read wherry-receiver's journal after the boundary"
	target=$(invocation) || fail "systemctl show failed for wherry-receiver"
	[ -n "$target" ] || return 1
	while IFS=$'\t' read -r id message; do
		[ -n "$id$message" ] || continue
		# The invocation the boundary saw began before it; the README's order lets it refuse the old binary.
		if [ -n "$before" ] && [ "$id" = "$before" ]; then continue; fi
		# The documented order leaves nothing for a second start to fix, so a retry after the boundary is a failure.
		[ -z "$id" ] || [ "$id" = "$target" ] ||
			fail "wherry-receiver invocation $id began after the boundary besides $target"
		if [[ $message == *" and the pin is "* ]]; then
			fail "wherry-receiver invocation ${id:-(none)} refused its plugin after the boundary: $message"
		fi
		line=$(grep -o -E '(^|[[:space:]])plugin systemd: version [^[:space:]]+ protocol [0-9]+$' <<<"$message" | sed 's/^[[:space:]]//' || true)
		[ -n "$line" ] || continue
		[ "$line" = "$want" ] || fail "wherry-receiver invocation ${id:-(none)} logged '$line', not '$want'"
		[ "$id" != "$target" ] || seen=1
	done <<<"$entries"
	[ -n "$target" ] && [ "$target" != "$before" ] && [ -n "$seen" ] || return 1
	fresh=$target
}

live() { host receivers | grep -E '^receiver=systemd-1 .* status=live$' >/dev/null; }

# started checks the start or restart that followed the boundary, as step 6 and the Upgrade's step 4 do: within 120
# seconds a fresh invocation logs the identity line of the plugin version $1 and protocol $2, with no pin refusal or
# other identity line in any invocation after the boundary; then the receiver shows live; then the same checks hold
# again and that invocation is still the unit's, so the receiver has not exited and restarted since.
started() {
	local first
	fresh=
	wait_for 120 evidence "$1" "$2" ||
		fail "no fresh wherry-receiver invocation logged 'plugin systemd: version $1 protocol $2' within 120 seconds"
	first=$fresh
	wait_for 300 live || fail "systemd-1 did not show live within 300 seconds"
	[ "$(invocation)" = "$first" ] || fail "wherry-receiver began another invocation after $first"
	evidence "$1" "$2" && [ "$fresh" = "$first" ] || fail "wherry-receiver invocation $first no longer holds its identity line"
	echo "invocation $first: plugin systemd: version $1 protocol $2; systemd-1 live"
}
