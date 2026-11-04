# shellcheck shell=bash disable=SC2034
# Sourced by each e2e step. The steps share their state in E2E_WORK, outside the checkout. They run as a user with root
# through sudo: the README's host commands run as root, and the operator and host commands as the user.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
e2e=$repo/systemd/test/e2e
work=${E2E_WORK:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wherry-systemd-e2e}
# The operator's working directory, and the host's own paths under $E2E_WORK/host. $E2E_WORK/bin holds the verified
# wherry CLI that provisions and submits.
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

# same_protocol succeeds when the previous release and the checkout serve the same plugin protocol. When they differ it
# fails after .github/plugin-protocol.sh's notice, and the lane skips its roll to and from the previous release. It
# fails the step on any other error, such as no previous release.
same_protocol() {
	local code=0
	"$repo/.github/plugin-protocol.sh" systemd || code=$?
	case $code in
	0) return 0 ;;
	2) echo "SKIP: $(basename "$0"): the previous release serves another plugin protocol" && return 1 ;;
	*) fail "the plugin protocol check exited $code" ;;
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

# invocation prints the receiver unit's current invocation ID.
invocation() { systemctl show -p InvocationID --value wherry-receiver; }

# journal prints the receiver unit's journal for its invocation $1 alone.
journal() { sudo journalctl -u wherry-receiver _SYSTEMD_INVOCATION_ID="$1" --no-pager -o cat; }

# identified succeeds once the receiver's invocation $1 has logged its plugin's identity line, and fails the step when
# that invocation refused its plugin for its pin.
identified() {
	local log
	log=$(journal "$1")
	if grep -F ' and the pin is ' <<<"$log" >/dev/null; then
		fail "wherry-receiver invocation $1 refused its plugin: $(grep -F -m 1 ' and the pin is ' <<<"$log")"
	fi
	grep -E '(^|[^[:alnum:]])plugin systemd: version [^ ]+ protocol [0-9]+' <<<"$log" >/dev/null
}

live() { host receivers | grep -E '^receiver=systemd-1 .* status=live$' >/dev/null; }

# started checks the receiver's invocation $1, which the step's start or restart began, as step 6 and the Upgrade's
# step 4 do: it logs the identity line of the plugin version $2 and no pin refusal, the receiver shows live, and $1 is
# still the unit's invocation, so the receiver has not exited and restarted since.
started() {
	local log
	[ -n "$1" ] || fail "wherry-receiver has no invocation"
	wait_for 120 identified "$1"
	log=$(journal "$1")
	grep -F "plugin systemd: version $2 protocol " <<<"$log" >/dev/null ||
		fail "wherry-receiver invocation $1 logged $(grep -E -o -m 1 'plugin systemd: version [^ ]+' <<<"$log"), not plugin systemd: version $2"
	wait_for 300 live
	[ "$(invocation)" = "$1" ] || fail "wherry-receiver restarted after invocation $1"
	if journal "$1" | grep -F ' and the pin is ' >/dev/null; then fail "wherry-receiver invocation $1 refused its plugin"; fi
	echo "invocation $1: $(grep -F -m 1 'plugin systemd: version ' <<<"$log")"
}

# complete submits a run with the prompt $1 as the host and waits for it, with its reply in $work/<run>.reply, its
# outputs in $work/outputs/<run> and its log in $work/<run>.log. It checks that the run completed, that the fake agent
# replied with its UID, and that the run returned the prompt as echo.txt. A pool takes submits once a receiver has
# taken custody and published the pool key.
complete() {
	local out run i code=0
	for i in $(seq 60); do
		out=$(host submit --selector e2e=fake --output echo.txt:required --prompt "$1" 2>&1) && break
		[[ $out == *"has no custodian yet"* ]] || break
		sleep 2
	done
	run=$(sed -n 's/^run_id=\([^ ]*\) .*/\1/p' <<<"$out")
	[ -n "$run" ] || fail "submit printed no run ID: $out"
	host wait "$run" --timeout 5m --outputs-dir "$work/outputs/$run" >"$work/$run.reply" || code=$?
	host logs "$run" >"$work/$run.log"
	[ "$code" = 0 ] || fail "run $run: wait exited $code; want 0, completed"
	grep -E -x 'uid [0-9]+' "$work/$run.reply" >/dev/null || fail "run $run did not reply with its UID"
	grep -F "$1" "$work/outputs/$run/echo.txt" >/dev/null || fail "run $run returned no echo.txt"
	echo "run $run completed"
}
