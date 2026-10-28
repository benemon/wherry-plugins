#!/usr/bin/env bash
# Rolls the receiver job back to the previous release's image and completes a run on it.
. "$(dirname "$0")/common.sh"
roll wherry-receiver:previous
run=$(submit 'e2e: rollback')
complete "$run" 'e2e: rollback'
echo "PASS"
