#!/usr/bin/env bash
# Rolls the receiver back to the previous release's plugin as the Upgrade's rollback sentence orders it, as root through
# sudo: apply the previous plugin pin, repoint current to the previous version and restart. Then checks the identity
# line, waits for live, and completes a run. When the previous release serves another plugin protocol, the step skips
# with a notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)

set_pin "$previous"
sudo ln -sfn "$previous" "$libexec/current"
sudo systemctl restart wherry-receiver
restarted=$(invocation)
started "$restarted" "$previous"

# Harness: a run on the rolled-back receiver.
complete 'e2e: rollback'
echo "PASS"
