#!/usr/bin/env bash
# Rolls the receiver to the previous release's image, then upgrades it to the checkout's, as kubernetes/README.md's
# Upgrade orders it, and completes a run on the upgraded receiver. Both images hold the wherry release WHERRY_VERSION,
# so the step upgrades the plugin alone; each roll first pins the receiver's record to the plugin binary of the image
# it runs, and checks the new pod logs the version it rolled to. When the previous release serves another plugin
# protocol, the step skips with a notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)
roll wherry-receiver:previous "$previous"
roll wherry-receiver:e2e e2e
run=$(submit e2e=fake --prompt 'e2e: upgrade' --output echo.txt:required)
finish "$run" TASK_STATE_COMPLETED
replied "$run" 'uid 1001'
grep -F 'e2e: upgrade' "$work/outputs/$run/echo.txt" >/dev/null || fail "run $run returned no echo.txt"
echo "PASS"
