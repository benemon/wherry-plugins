# shellcheck shell=bash disable=SC2034
# Sourced by each e2e step. The steps share their state in E2E_WORK, outside the checkout, so no build context holds it.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
e2e=$repo/nomad/test/e2e
work=${E2E_WORK:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wherry-nomad-e2e}
job=$repo/examples/nomad/wherry-receiver.nomad.hcl
host_job=$repo/examples/nomad/wherry-receiver-host.nomad.hcl
host_config=$work/host/config.yaml
export NOMAD_ADDR=http://127.0.0.1:4646 PATH=$work/bin:$PATH

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

api() { curl -fsS "$NOMAD_ADDR/v1/$1"; }
host() { wherry "$1" --config-file "$host_config" "${@:2}"; }
ready() { host receivers | grep 'profile=fake plugin=nomad agent=fake ready=true' >/dev/null; }

# deployed succeeds once the receiver job's current version has deployed, and fails the step if it cannot.
deployed() {
	local version status
	version=$(api job/wherry-receiver | jq .Version)
	status=$(api job/wherry-receiver/deployment | jq -r --argjson v "$version" 'select(.JobVersion == $v) | .Status')
	[ "$status" != failed ] || fail "the receiver job's version $version did not deploy"
	[ "$status" = successful ]
}

# plugin_sha256 prints the SHA-256 of the plugin binary in the receiver image $1, the pin receiver add and update take.
plugin_sha256() {
	docker run --rm --entrypoint sha256sum "$1" /usr/local/libexec/wherry/plugins/wherry-plugin-nomad | cut -d' ' -f1
}

# pin records the receiver's configuration with the plugin binary of the receiver image $1, which a receiver must run:
# it refuses a plugin whose SHA-256 is not its record's. A running receiver exits on the new record.
pin() {
	(
		# shellcheck source=/dev/null
		. "$work/kafka/env"
		wherry admin receiver update nomad-1 --config-dir "$e2e/receiver" --plugin-sha256 "nomad=$(plugin_sha256 "$1")"
	)
}

# roll pins the image $1's plugin, runs the receiver job with the image and waits for it to deploy.
roll() {
	pin "$1"
	nomad job run -detach -var image="$1" "$job" >/dev/null
	wait_for 300 deployed
	wait_for 300 ready
}

submit() {
	local run
	run=$(host submit --selector e2e=fake --output echo.txt:required --prompt "$1" | sed -n 's/^run_id=\([^ ]*\) .*/\1/p')
	[ -n "$run" ] || fail "submit printed no run ID"
	echo "$run"
}

# complete waits for the run $1, with its reply in $work/<run>.reply, its outputs in $work/outputs/<run> and its log in
# $work/<run>.log, and checks that it completed with the agent's reply and its prompt as echo.txt. wait prints the reply
# and exits 0 for a completed run, 2 failed, 3 canceled, 4 rejected, 6 timed out.
complete() {
	local code=0
	host wait "$1" --timeout 5m --outputs-dir "$work/outputs/$1" >"$work/$1.reply" || code=$?
	host logs "$1" >"$work/$1.log"
	[ "$code" = 0 ] || fail "run $1: wait exited $code; want 0, completed"
	grep -x -F 'uid 1001' "$work/$1.reply" >/dev/null || fail "run $1 did not reply 'uid 1001'"
	grep -F "$2" "$work/outputs/$1/echo.txt" >/dev/null || fail "run $1 returned no echo.txt"
}
