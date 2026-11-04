#!/usr/bin/env bash
# Rolls the receiver back to the previous release's plugin as the Upgrade's rollback sentence orders it, as root through
# sudo: apply the previous plugin pin, repoint current to the previous version and restart. Then checks the identity
# line in every invocation since the boundary, just before the restart, and waits for live, as step 4 does. When
# 02-fetch.sh's protocol check found no plugin protocol that the previous release and the checkout both serve, the step
# skips with the check's notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
previous=$(previous_version)
protocol=$(protocol previous)

set_pin "$previous"
sudo ln -sfn "$previous" "$libexec/current"
boundary
sudo systemctl restart wherry-receiver
started "$previous" "$protocol"
echo "PASS"
