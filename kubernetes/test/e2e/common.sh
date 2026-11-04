# shellcheck shell=bash disable=SC2034
# Sourced by each e2e step. The steps share their state in E2E_WORK, outside the checkout, so no build context holds it.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
e2e=$repo/kubernetes/test/e2e
work=${E2E_WORK:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wherry-e2e}
cluster=wherry-e2e
namespace=wherry
# The operator's working directory, and the host's own paths under $E2E_WORK/host.
operator=$work/operator
export KUBECONFIG=$work/kubeconfig PATH=$work/bin:$PATH XDG_CONFIG_HOME=$work/host/config XDG_STATE_HOME=$work/host/state
host_config=$XDG_CONFIG_HOME/wherry/config.yaml
# 04-deploy.sh's copy of examples/kubernetes, which kubectl apply -k applies.
deploy=$work/deploy

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
ready() { host receivers | grep 'profile=fake plugin=kubernetes agent=fake ready=true' >/dev/null; }

# A pool takes submits once a receiver has taken custody and published the pool key, a few seconds after it starts.
submit() {
	local out run i
	for i in $(seq 60); do
		out=$(host submit --selector "$@" 2>&1) && break
		[[ $out == *"has no custodian yet"* ]] || break
		sleep 2
	done
	run=$(sed -n 's/^run_id=\([^ ]*\) .*/\1/p' <<<"$out")
	[ -n "$run" ] || fail "submit printed no run ID: $out"
	echo "$run"
}
# wait's exit code for each state a run ends in; 6 is its timeout.
declare -A codes=([TASK_STATE_COMPLETED]=0 [TASK_STATE_FAILED]=2 [TASK_STATE_CANCELED]=3 [TASK_STATE_REJECTED]=4)
# finish waits for a run, with its reply in $work/<run>.reply, its outputs in $work/outputs/<run> and its log in
# $work/<run>.log, and checks that it ended in the state $2 by wait's exit code.
finish() {
	local code=0
	host wait "$1" --timeout 5m --outputs-dir "$work/outputs/$1" >"$work/$1.reply" || code=$?
	host logs "$1" >"$work/$1.log"
	[ "$code" = "${codes[$2]}" ] || fail "run $1: wait exited $code; want ${codes[$2]}, $2"
}
replied() { grep -x -F "$2" "$work/$1.reply" >/dev/null || fail "run $1 did not reply '$2'"; }

# plugin_sha256 prints the SHA-256 of the plugin binary in the receiver image $1, the pin of the receiver's file.
plugin_sha256() {
	docker run --rm --entrypoint sha256sum "$1" /usr/local/libexec/wherry/plugins/wherry-plugin-kubernetes | cut -d' ' -f1
}

# pin applies the receiver configuration directory test/e2e/receiver, with the plugin binary of the receiver image $1
# as its pin, to the receiver kind-1, creating it on the first call. A receiver refuses a plugin whose SHA-256 is not
# its record's pin, and a running receiver exits on a new record.
pin() {
	local sha256
	sha256=$(plugin_sha256 "$1")
	rm -rf "$work/receiver"
	cp -R "$e2e/receiver" "$work/receiver"
	printf 'plugin_sha256:\n  kubernetes: %s\n' "$sha256" >>"$work/receiver/receiver.yaml"
	wherry operator receiver apply kind-1 --dir "$operator" -f "$work/receiver"
}

# pinned fails the step unless the receiver kind-1's record, as receiver show prints it, pins the plugin binary of the
# receiver image $1.
pinned() {
	local recorded want
	recorded=$(wherry operator receiver show kind-1 --dir "$operator" |
		awk '/^plugin_sha256:/ { pins = 1; next } /^[^ ]/ { pins = 0 } pins && $1 == "kubernetes:" { print $2 }')
	want=$(plugin_sha256 "$1")
	[ "$recorded" = "$want" ] || fail "the receiver's record pins '$recorded', not $1's plugin $want"
}

# pod prints the name of the newest receiver pod, not being deleted, whose receiver container runs the image $1.
pod() {
	kubectl get pods -n "$namespace" -l app=wherry-receiver -o json | jq -r --arg image "$1" '.items |
		map(select(.metadata.deletionTimestamp == null and any(.spec.containers[]; .name == "receiver" and .image == $image))) |
		sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty'
}

# rolled succeeds once the receiver Deployment has rolled out a pod of the image $1 that has logged its plugin identity
# line and the receiver is ready, and fails the step as soon as that pod's receiver refuses the image's plugin for its
# pin.
rolled() {
	local name
	name=$(pod "$1")
	[ -n "$name" ] || return 1
	if kubectl logs -n "$namespace" "$name" -c receiver 2>/dev/null | grep -F ' and the pin is ' >/dev/null; then
		fail "pod $name's receiver refused $1's plugin: the record does not pin it"
	fi
	kubectl logs -n "$namespace" "$name" -c receiver 2>/dev/null | grep -F 'plugin kubernetes: version ' >/dev/null || return 1
	kubectl rollout status -n "$namespace" deployment/wherry-receiver --timeout=10s >/dev/null && ready
}

# identified fails the step unless the receiver log on standard input, named $3 in errors, logs the identity line of the
# plugin version $1 and protocol $2, and no other identity line.
identified() {
	local want="plugin kubernetes: version $1 protocol $2" lines
	lines=$(grep -o -E '(^|[[:space:]])plugin kubernetes: version [^[:space:]]+ protocol [0-9]+$' | sed 's/^[[:space:]]//' || true)
	[ -n "$lines" ] || fail "$3 logged no identity line; want '$want'"
	if grep -v -x -F "$want" <<<"$lines" >/dev/null; then
		fail "$3 logged '$(grep -v -x -F -m 1 "$want" <<<"$lines")', not '$want'"
	fi
}

# roll upgrades or rolls back the receiver as kubernetes/README.md orders it: it pins the image $1's plugin, then, once
# the record holds that pin, sets the image's tag in the Kustomize copy, applies it and waits for the rollout. The new
# pod's receiver must log the identity line of the plugin version $2 and protocol $3, must never have refused its pin,
# and must not have restarted; the step reads that pod's log alone.
roll() {
	local name log restarts
	pin "$1"
	pinned "$1"
	sed -i "s|newName: wherry-receiver, newTag: [^}]*}|newName: wherry-receiver, newTag: ${1#*:}}|" "$deploy/kustomization.yaml"
	grep -F "newName: wherry-receiver, newTag: ${1#*:}}" "$deploy/kustomization.yaml" >/dev/null ||
		fail "kustomization.yaml does not set the image $1"
	kubectl apply -k "$deploy" >/dev/null
	wait_for 300 rolled "$1" || fail "the receiver Deployment did not roll out $1 with a ready receiver"
	name=$(pod "$1")
	log=$(kubectl logs -n "$namespace" "$name" -c receiver)
	if grep -F ' and the pin is ' <<<"$log" >/dev/null; then
		fail "pod $name's receiver ran $1's plugin under another pin"
	fi
	identified "$2" "$3" "pod $name's receiver" <<<"$log"
	restarts=$(kubectl get pod -n "$namespace" "$name" -o jsonpath='{.status.containerStatuses[?(@.name=="receiver")].restartCount}')
	[ "$restarts" = 0 ] || fail "pod $name's receiver restarted $restarts times"
	echo "pod $name: plugin kubernetes: version $2 protocol $3"
}

# previous_version prints the plugin version of the previous release, which 02-wherry.sh builds as
# wherry-receiver:previous.
previous_version() { cat "$work/previous-version"; }

# protocol_check runs .github/plugin-protocol.sh for the kubernetes plugin and keeps its result lines in
# $work/protocol.env: the previous release's tag, the protocols each side serves, and result=run or result=skip. Any failure of the check
# fails the step; a skip is only ever the check's result=skip, never an error.
protocol_check() {
	"$repo/.github/plugin-protocol.sh" kubernetes | tee "$work/protocol.out"
	grep -E '^(previous|previous_protocols|current_protocols|result)=' "$work/protocol.out" >"$work/protocol.env"
}

# checked prints the value of the key $1 that protocol_check kept, and fails the step when it has none.
checked() {
	local value
	value=$(sed -n "s/^$1=//p" "$work/protocol.env" 2>/dev/null || true)
	[ -n "$value" ] || fail "02-wherry.sh's protocol check kept no $1"
	echo "$value"
}

# protocol prints the plugin protocol the receiver runs with the side $1, previous or current: the newest it serves.
protocol() {
	local protocols
	protocols=$(checked "$1_protocols") || exit 1
	tr ' ' '\n' <<<"$protocols" | sort -n | tail -n 1
}

# same_protocol succeeds when protocol_check found protocols both sides serve. When it found none it repeats the check's
# notice and fails, and the step skips its roll to the previous release. Any other value fails the step.
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
