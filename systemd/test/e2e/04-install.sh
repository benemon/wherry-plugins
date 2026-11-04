#!/usr/bin/env bash
# Runs steps 1 to 6 of systemd/README.md's Run the receiver as a systemd service, as root through sudo, with the
# previous release's plugin, so 05-upgrade.sh upgrades from it; when 02-fetch.sh's protocol check found no plugin
# protocol that the previous release and the checkout both serve, with the checkout's. It installs on a host with no
# receiver yet, as the disposable CI runner is: step 1's links fail where they exist.
. "$(dirname "$0")/common.sh"
if same_protocol; then
	version=$(previous_version)
	protocol=$(protocol previous)
	from=$work/previous
else
	version=e2e
	protocol=$(protocol current)
	from=$work/current
fi
echo "installing plugin version $version, protocol $protocol"

# Step 1: install the binaries in a directory for this deployment version. Ubuntu has no SELinux, so no restorecon.
sudo install -d -m 0755 "$libexec/$version/plugins"
sudo install -m 0755 "$work/wherry/wherry" "$libexec/$version/wherry"
sudo install -m 0755 "$from/wherry-plugin-systemd" "$libexec/$version/plugins/"
sudo ln -s "$version" "$libexec/current"
sudo ln -s "$libexec/current/wherry" /usr/local/bin/wherry

# Step 2: the plugin pin, which for a release binary matches its entry in the verified checksums.
pin=$(sha256sum "$libexec/$version/plugins/wherry-plugin-systemd" | cut -d' ' -f1)
if [ "$version" != e2e ]; then
	listed=$(awk -v name="wherry-plugin-systemd_${version}_linux_$(arch)" '$2 == name { print $1 }' "$work/previous/checksums.txt")
	[ "$pin" = "$listed" ] || fail "the installed plugin's SHA-256 $pin is not its checksums.txt entry '$listed'"
fi

# Step 3: the receiver's file with the pin, and the example config.yaml with ca_file set for the broker's private CA,
# then receiver apply.
mkdir -p "$operator/receivers"
cp "$e2e/receiver.yaml" "$operator/receivers/systemd-1.yaml"
sed 's|^# ca_file: |ca_file: |' "$repo/examples/systemd/config.yaml" >"$operator/receivers/systemd-1.config.yaml"
grep -x 'ca_file: /etc/wherry/kafka-ca.pem' "$operator/receivers/systemd-1.config.yaml" >/dev/null ||
	fail "config.yaml names no ca_file"
set_pin "$version"
[ "$(sha256sum "$libexec/$version/plugins/wherry-plugin-systemd" | cut -d' ' -f1)" = "$pin" ] || fail "the pin changed"
mask "$(sed -n 's/^password: *//p' "$operator/bundles/receivers/systemd-1.bundle" | tr -d "\"'")"

# Step 4: the agents' state roots, the units and the tmpfiles configuration.
sudo install -d -m 0700 /var/lib/private
sudo install -d -m 0755 /var/lib/private/wherry-agents /var/lib/wherry-agents /wherry
sudo install -m 0644 "$repo/examples/systemd/wherry.slice" "$repo/examples/systemd/wherry-receiver.service" /etc/systemd/system/
sudo install -m 0644 "$repo/examples/systemd/wherry-tmpfiles.conf" /etc/tmpfiles.d/wherry.conf
sudo systemd-tmpfiles --create /etc/tmpfiles.d/wherry.conf

# Step 5: the bundle, the local configuration and the broker's CA at its ca_file.
sudo install -D -m 0600 "$operator/bundles/receivers/systemd-1.bundle" /etc/wherry/receiver.bundle
sudo install -m 0600 "$operator/receivers/systemd-1.config.yaml" /etc/wherry/config.yaml
sudo install -m 0644 "$work/kafka/ca.crt" /etc/wherry/kafka-ca.pem
sysctl kernel.core_pattern

# Step 6: start the receiver, check its identity line in every invocation since the boundary, and wait for it to show
# live.
sudo systemctl daemon-reload
boundary
sudo systemctl enable --now wherry-receiver
started "$version" "$protocol"
echo "PASS"
