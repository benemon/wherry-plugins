# shellcheck shell=bash disable=SC2034
# Sourced by each e2e step. The steps share their state in E2E_WORK, outside the checkout, so no build context holds it.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
e2e=$repo/nomad/test/e2e
work=${E2E_WORK:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wherry-nomad-e2e}
job=$repo/examples/nomad/wherry-receiver.nomad.hcl
host_job=$repo/examples/nomad/wherry-receiver-host.nomad.hcl
# The operator's working directory, and the host's own paths under $E2E_WORK/host.
operator=$work/operator
export NOMAD_ADDR=http://127.0.0.1:4646 PATH=$work/bin:$PATH XDG_CONFIG_HOME=$work/host/config \
	XDG_STATE_HOME=$work/host/state

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
host() { wherry host "$@"; }
ready() { host receivers | grep 'profile=fake plugin=nomad agent=fake ready=true' >/dev/null; }

# deployed succeeds once the receiver job's current version has deployed, and fails the step if it cannot.
deployed() {
	local version status
	version=$(api job/wherry-receiver | jq .Version)
	status=$(api job/wherry-receiver/deployment | jq -r --argjson v "$version" 'select(.JobVersion == $v) | .Status')
	[ "$status" != failed ] || fail "the receiver job's version $version did not deploy"
	[ "$status" = successful ]
}

# plugin_sha256 prints the SHA-256 of the plugin binary in the receiver image $1, the pin of the receiver's file.
plugin_sha256() {
	docker run --rm --entrypoint sha256sum "$1" /usr/local/libexec/wherry/plugins/wherry-plugin-nomad | cut -d' ' -f1
}

# pin applies the receiver configuration directory test/e2e/receiver, with the plugin binary of the receiver image $1
# as its pin, to the receiver nomad-1, creating it on the first call. A receiver refuses a plugin whose SHA-256 is not
# its record's pin, and a running receiver exits on a new record.
pin() {
	rm -rf "$work/receiver"
	cp -R "$e2e/receiver" "$work/receiver"
	printf 'plugin_sha256:\n  nomad: %s\n' "$(plugin_sha256 "$1")" >>"$work/receiver/receiver.yaml"
	wherry operator receiver apply nomad-1 --dir "$operator" -f "$work/receiver"
}

# pinned fails the step unless the receiver nomad-1's record, as receiver show prints it, pins the plugin binary of the
# receiver image $1.
pinned() {
	local recorded want
	recorded=$(wherry operator receiver show nomad-1 --dir "$operator" |
		awk '/^plugin_sha256:/ { pins = 1; next } /^[^ ]/ { pins = 0 } pins && $1 == "nomad:" { print $2 }')
	want=$(plugin_sha256 "$1")
	[ "$recorded" = "$want" ] || fail "the receiver's record pins '$recorded', not $1's plugin $want"
}

# newest prints the ID of the receiver job's newest running allocation.
newest() {
	api job/wherry-receiver/allocations | jq -r \
		'map(select(.DesiredStatus == "run" and .ClientStatus == "running")) | sort_by(.CreateIndex) | last | .ID // empty'
}

# roll upgrades or rolls back the receiver as nomad/README.md orders it: it pins the image $1's plugin, then, once the
# record holds that pin, runs the receiver job with the image and waits for it to deploy. The new allocation's receiver
# must never have run under another pin.
roll() {
	local alloc
	pin "$1"
	pinned "$1"
	nomad job run -detach -var image="$1" "$job" >/dev/null
	wait_for 300 deployed
	wait_for 300 ready
	alloc=$(newest)
	[ -n "$alloc" ] || fail "the receiver job has no running allocation"
	if { nomad alloc logs "$alloc" receiver && nomad alloc logs -stderr "$alloc" receiver; } |
		grep -F ' and the pin is ' >/dev/null; then
		fail "allocation $alloc's receiver ran $1's plugin under another pin"
	fi
}

# metrics_address prints the client address and port of the running receiver allocation's metrics port, which the
# bridge network maps to the receiver's 9464.
metrics_address() {
	local alloc
	alloc=$(newest)
	[ -n "$alloc" ] || fail "the receiver job has no running allocation"
	api "allocation/$alloc" |
		jq -r '.AllocatedResources.Shared.Ports[] | select(.Label == "metrics") | "\(.HostIP):\(.Value)"'
}

# A pool takes submits once a receiver has taken custody and published the pool key, a few seconds after it starts.
submit() {
	local out run i
	for i in $(seq 60); do
		out=$(host submit --selector e2e=fake --output echo.txt:required --prompt "$1" 2>&1) && break
		[[ $out == *"has no custodian yet"* ]] || break
		sleep 2
	done
	run=$(sed -n 's/^run_id=\([^ ]*\) .*/\1/p' <<<"$out")
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
