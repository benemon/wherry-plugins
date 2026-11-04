#!/usr/bin/env bash
# Collects the receiver unit's journal and status, the agent slice's status, the last restart boundary, the plugin
# protocol check's output and the broker container's log into $E2E_WORK/artifacts after a failed step. It reads no
# bundle, credential or configuration file, and it exits 0 without changing the failed step's result.
. "$(dirname "$0")/common.sh"
set +e
out=$work/artifacts
mkdir -p "$out"
# shellcheck disable=SC2024
sudo journalctl -u wherry-receiver --no-pager >"$out/receiver.log" 2>&1
systemctl status --no-pager wherry-receiver >"$out/receiver-status.txt" 2>&1
systemctl status --no-pager wherry.slice 'wherry-agent-*' >"$out/agents-status.txt" 2>&1
ls -l "$libexec" >"$out/versions.txt" 2>&1
cp "$work/boundary" "$work/protocol.out" "$out/" 2>/dev/null
docker logs wherry-kafka >"$out/kafka.log" 2>&1
exit 0
