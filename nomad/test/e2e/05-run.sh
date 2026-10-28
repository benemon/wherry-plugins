#!/usr/bin/env bash
# Submits a run as the host runner to the checkout's receiver and watches it, with its outputs.
. "$(dirname "$0")/common.sh"
run=$(submit 'e2e: plain')
complete "$run" 'e2e: plain'
echo "PASS"
