#!/usr/bin/env bash
# Rolls the receiver to the previous release's image, then upgrades it to the checkout's, as kubernetes/README.md's
# Upgrade orders it, and completes a run on the upgraded receiver. Both images hold the wherry release WHERRY_VERSION,
# so the step upgrades the plugin alone; each roll first pins the receiver's record to the plugin binary of the image
# it runs, and checks the new pod logs the identity line of the version and protocol it rolled to. When 02-wherry.sh's
# protocol check found no plugin protocol that the previous release and the checkout both serve, the step skips with
# its notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)
previous_protocol=$(protocol previous)
current_protocol=$(protocol current)
roll wherry-receiver:previous "$previous" "$previous_protocol"
roll wherry-receiver:e2e e2e "$current_protocol"
run=$(submit e2e=fake --prompt 'e2e: upgrade' --output echo.txt:required)
finish "$run" TASK_STATE_COMPLETED
replied "$run" 'uid 1001'
grep -F 'e2e: upgrade' "$work/outputs/$run/echo.txt" >/dev/null || fail "run $run returned no echo.txt"
echo "PASS"
