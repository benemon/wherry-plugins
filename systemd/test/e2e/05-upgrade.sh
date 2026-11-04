#!/usr/bin/env bash
# Upgrades the receiver from the previous release's plugin to the checkout's with the Upgrade steps of
# systemd/README.md's Run the receiver as a systemd service, as root through sudo. Both versions hold the wherry release
# WHERRY_VERSION, so the step upgrades the plugin alone. When 02-fetch.sh's protocol check found no plugin protocol that
# the previous release and the checkout both serve, 04-install.sh installed the checkout's, and the step skips with the
# check's notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0
protocol=$(protocol current)

# Step 1: install the new binaries beside the old version, and obtain the new pin.
sudo install -d -m 0755 "$libexec/e2e/plugins"
sudo install -m 0755 "$work/wherry/wherry" "$libexec/e2e/wherry"
sudo install -m 0755 "$work/current/wherry-plugin-systemd" "$libexec/e2e/plugins/"

# Step 2: update plugin_sha256.systemd and apply. Until the new version runs, the receiver's restarts refuse the old
# plugin; those refusals come before the boundary.
set_pin e2e

# Step 3: immediately repoint current and restart. The boundary, just before the restart, is where step 4's evidence
# starts.
sudo ln -sfn e2e "$libexec/current"
boundary
sudo systemctl restart wherry-receiver

# Step 4: the identity line in every invocation since the boundary, and live.
started e2e "$protocol"
echo "PASS"
