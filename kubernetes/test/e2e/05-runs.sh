#!/usr/bin/env bash
# Submits and watches, as the host runner, a cancelled run, a plain run, a credentialed run with an input file and an
# SSH private key, a run with an input file and a run that never starts, and checks what each brings back. Ends once
# the sweep has removed the cancelled run's pod.
. "$(dirname "$0")/common.sh"
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
host() { wherry "$1" --config-file "$host_config" "${@:2}"; }
submit() {
	local run
	run=$(host submit --selector "$@" | sed -n 's/^run_id=\([^ ]*\) .*/\1/p')
	[ -n "$run" ] || fail "submit printed no run ID"
	echo "$run"
}
# finish watches a run into $work/<run>.watch and its outputs into $work/outputs/<run>, and checks the state it ended
# in. watch prints the transcript before the state line.
finish() {
	local state
	wherry watch "$1" --config-file "$host_config" --timeout 5m --outputs-dir "$work/outputs/$1" >"$work/$1.watch" || true
	state=$(grep -x 'TASK_STATE_[A-Z_]*' "$work/$1.watch" | tail -n 1)
	[ "$state" = "$2" ] || fail "run $1 ended $state; want $2"
}
replied() { grep -x -F "$2" "$work/$1.watch" >/dev/null || fail "run $1 did not reply '$2'"; }
sha256() { sha256sum <"$1" | cut -d' ' -f1; }
ready() { host receivers | grep 'profile=fake plugin=kubernetes agent=fake ready=true' >/dev/null; }
running() {
	kubectl get pod -n "$namespace" -l "wherry.io/run=$cancelled" 2>/dev/null \
		-o jsonpath='{.items[*].status.containerStatuses[?(@.name=="agent")].state.running.startedAt}' | grep . >/dev/null
}
gone() {
	local names
	names=$(kubectl get "$1" -n "$namespace" -l "wherry.io/run=$cancelled" -o name) && [ -z "$names" ]
}

wait_for 300 ready

# Cancelled first, since the sweep removes its pod 10 minutes after it ends.
cancelled=$(submit e2e=fake --prompt 'e2e: sleep')
wait_for 300 running
host cancel "$cancelled"
finish "$cancelled" TASK_STATE_CANCELED
wait_for 120 gone secrets

plain=$(submit e2e=fake --prompt 'e2e: plain' --output echo.txt:required)
finish "$plain" TASK_STATE_COMPLETED
replied "$plain" 'uid 1001'
grep -F 'e2e: plain' "$work/outputs/$plain/echo.txt" >/dev/null || fail "run $plain returned no echo.txt"

head -c 4096 /dev/urandom >"$work/input.bin"
credentialed=$(submit e2e=fake --prompt 'e2e: credential' --input "$work/input.bin" \
	--credential dummy:file=token --credential deploy:file=id_ed25519)
finish "$credentialed" TASK_STATE_COMPLETED
replied "$credentialed" "file token 440 0 $(sha256 "$work/host/dummy")"
replied "$credentialed" "file id_ed25519 440 0 $(sha256 "$work/host/deploy-key")"
replied "$credentialed" "file input.bin 600 1001 $(sha256 "$work/input.bin")"
replied "$credentialed" 'ssh-keygen accepted id_ed25519'

input=$(submit e2e=fake --prompt 'e2e: input' --input "$work/input.bin")
finish "$input" TASK_STATE_COMPLETED
replied "$input" "file input.bin 600 1001 $(sha256 "$work/input.bin")"

broken=$(submit e2e=broken --prompt 'e2e: start failure')
finish "$broken" TASK_STATE_FAILED
kubectl logs -n "$namespace" deployment/wherry-receiver >"$work/receiver.log"
grep -F "run $broken: job never started (image): " "$work/receiver.log" >/dev/null ||
	fail "the receiver logged no image start failure for run $broken"

# The dummy credential appears in no log, pod or event.
kubectl get pods,events -n "$namespace" -o yaml >"$work/objects.yaml"
for file in "$work/receiver.log" "$work/objects.yaml"; do
	if grep -F "$(cat "$work/host/dummy")" "$file" >/dev/null; then fail "the dummy credential is in $file"; fi
	if grep -F "$(sed -n 2p "$work/host/deploy-key")" "$file" >/dev/null; then fail "the SSH private key is in $file"; fi
done

wait_for 900 gone pods
echo "PASS"
