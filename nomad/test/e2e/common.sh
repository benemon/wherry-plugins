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

# roll runs the receiver job with the image $1 and waits for it to deploy.
roll() {
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

# complete watches the run $1 into $work/<run>.watch and its outputs into $work/outputs/<run>, and checks that it
# completed with the agent's reply and its prompt as echo.txt. watch prints the transcript before the state line.
complete() {
	local state
	wherry watch "$1" --config-file "$host_config" --timeout 5m --outputs-dir "$work/outputs/$1" >"$work/$1.watch" || true
	state=$(grep -x 'TASK_STATE_[A-Z_]*' "$work/$1.watch" | tail -n 1)
	[ "$state" = TASK_STATE_COMPLETED ] || fail "run $1 ended $state; want TASK_STATE_COMPLETED"
	grep -x -F 'uid 1001' "$work/$1.watch" >/dev/null || fail "run $1 did not reply 'uid 1001'"
	grep -F "$2" "$work/outputs/$1/echo.txt" >/dev/null || fail "run $1 returned no echo.txt"
}
