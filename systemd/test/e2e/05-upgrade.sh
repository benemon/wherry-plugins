#!/usr/bin/env bash
# Upgrades the receiver from the previous release's plugin to the checkout's with the Upgrade steps of
# systemd/README.md's Run the receiver as a systemd service, as root through sudo, and completes a run on it. Both
# versions hold the wherry release WHERRY_VERSION, so the step upgrades the plugin alone. When the previous release
# serves another plugin protocol, 04-install.sh installed the checkout's, and the step skips with a notice.
. "$(dirname "$0")/common.sh"
same_protocol || exit 0

# Step 1: install the new binaries beside the old version, and obtain the new pin.
sudo install -d -m 0755 "$libexec/e2e/plugins"
sudo install -m 0755 "$work/wherry/wherry" "$libexec/e2e/wherry"
sudo install -m 0755 "$work/current/wherry-plugin-systemd" "$libexec/e2e/plugins/"

# Step 2: update plugin_sha256.systemd and apply. Until the new version runs, the receiver's restarts refuse the old
# plugin.
set_pin e2e

# Step 3: immediately repoint current and restart. The invocation the restart began is the one step 4 checks.
sudo ln -sfn e2e "$libexec/current"
sudo systemctl restart wherry-receiver
restarted=$(invocation)

# Step 4: the identity line, and live.
started "$restarted" e2e

# Harness: a run on the upgraded receiver.
complete 'e2e: upgrade'
echo "PASS"
