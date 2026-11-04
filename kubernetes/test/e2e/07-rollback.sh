#!/usr/bin/env bash
# Rolls the receiver back to the previous release's image, as kubernetes/README.md's Upgrade says to roll back, pinned
# to its plugin binary, checks the new pod logs the previous release's version, and completes a run on it. When the
# previous release serves another plugin protocol, the step skips with a notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)
roll wherry-receiver:previous "$previous"
run=$(submit e2e=fake --prompt 'e2e: rollback' --output echo.txt:required)
finish "$run" TASK_STATE_COMPLETED
replied "$run" 'uid 1001'
grep -F 'e2e: rollback' "$work/outputs/$run/echo.txt" >/dev/null || fail "run $run returned no echo.txt"
echo "PASS"
