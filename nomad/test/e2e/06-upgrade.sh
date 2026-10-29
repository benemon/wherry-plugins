#!/usr/bin/env bash
# Rolls the receiver job to the previous release's image, submits a run whose agent replies after 90 seconds, and once
# its agent task runs, rolls the job to the checkout's image. The new receiver must adopt the agent job, not dispatch
# it again, and complete the run. Both images hold the wherry release WHERRY_VERSION, so the step upgrades the plugin
# alone; each roll first pins the receiver's record to the plugin binary of the image it runs. The upgraded receiver's
# metrics are on the client port its allocation maps the metrics port to.
. "$(dirname "$0")/common.sh"
roll wherry-receiver:previous
run=$(submit 'e2e: slow')
agent_job=agent-$(printf e2e | sha256sum | cut -c1-12)-$run
running() { api "job/$agent_job/allocations" | jq -e 'any(.[]; .TaskStates.agent.State == "running")' >/dev/null; }
wait_for 300 running
roll wherry-receiver:e2e
complete "$run" 'e2e: slow'
metrics=$(metrics_address)
[ -n "$metrics" ] || fail "the receiver allocation maps no metrics port"
curl -fsS "http://$metrics/metrics" >"$work/metrics.txt"
grep -E '^wherry_adoptions_total\{.*profile="fake".*\} [1-9]' "$work/metrics.txt" >/dev/null ||
	fail "the upgraded receiver adopted no run"
[ "$(api "job/$agent_job/allocations" | jq length)" = 1 ] || fail "run $run's agent job ran more than once"
echo "PASS"
