#!/usr/bin/env bash
# Rolls the receiver back to the previous release's image, as kubernetes/README.md's Upgrade says to roll back, pinned
# to its plugin binary, checks the new pod logs the identity line of the previous release's version and protocol, and
# completes a run on it. When 02-wherry.sh's protocol check found no plugin protocol that the previous release and the
# checkout both serve, the step skips with its notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)
previous_protocol=$(protocol previous)
roll wherry-receiver:previous "$previous" "$previous_protocol"
run=$(submit e2e=fake --prompt 'e2e: rollback' --output echo.txt:required)
finish "$run" TASK_STATE_COMPLETED
replied "$run" 'uid 1001'
grep -F 'e2e: rollback' "$work/outputs/$run/echo.txt" >/dev/null || fail "run $run returned no echo.txt"
echo "PASS"
