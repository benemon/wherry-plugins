#!/usr/bin/env bash
# Submits a run as the host runner to the checkout's receiver and watches it, with its outputs, then a run with no
# selector, which the one profile's signature takes.
. "$(dirname "$0")/common.sh"
run=$(submit 'e2e: plain')
complete "$run" 'e2e: plain'
unselected=$(submit 'e2e: no selector' '')
complete "$unselected" 'e2e: no selector'
echo "PASS"
