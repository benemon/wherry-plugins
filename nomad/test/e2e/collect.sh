#!/usr/bin/env bash
# Collects the Nomad agent's log, the receiver's log and the jobs' state into $E2E_WORK/artifacts after a failed step.
# It reads no variable.
. "$(dirname "$0")/common.sh"
set +e
out=$work/artifacts
mkdir -p "$out"
# shellcheck disable=SC2024
sudo journalctl -u wherry-nomad --no-pager >"$out/nomad.log" 2>&1
nomad alloc logs -job wherry-receiver receiver >"$out/receiver.log" 2>&1
nomad alloc logs -stderr -job wherry-receiver receiver >"$out/receiver.stderr.log" 2>&1
nomad job status -all-allocs wherry-receiver >"$out/receiver-job.txt" 2>&1
api 'jobs?meta=true' >"$out/jobs.json" 2>&1
api allocations >"$out/allocations.json" 2>&1
docker ps -a >"$out/docker.txt" 2>&1
docker logs wherry-kafka >"$out/kafka.log" 2>&1
exit 0
