#!/usr/bin/env bash
# Rolls the receiver job back to the previous release's image, pinned to its plugin binary, checks the new allocation
# logs the identity line of the previous release's version and protocol, and completes a run on it. When 02-wherry.sh's
# protocol check found no plugin protocol that the previous release and the checkout both serve, the step skips with
# its notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)
previous_protocol=$(protocol previous)
roll wherry-receiver:previous "$previous" "$previous_protocol"
run=$(submit 'e2e: rollback')
complete "$run" 'e2e: rollback'
echo "PASS"
