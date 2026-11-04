#!/usr/bin/env bash
# Rolls the receiver job back to the previous release's image, pinned to its plugin binary, checks the new allocation
# logs the previous release's version, and completes a run on it. When the previous release serves another plugin
# protocol, the step skips with a notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)
roll wherry-receiver:previous "$previous"
run=$(submit 'e2e: rollback')
complete "$run" 'e2e: rollback'
echo "PASS"
