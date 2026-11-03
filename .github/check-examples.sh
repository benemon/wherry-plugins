#!/usr/bin/env bash
# Checks the receiver examples of the plugins named as arguments: each drain_timeout stays at least 10 seconds below
# its platform's stop window, since a stop takes up to drain_timeout plus 5 seconds, the Kubernetes receiver limits the
# volume ownership pass, and the Nomad jobs render the bundle from their Nomad variable. Then fails on a shell script
# in examples/, and on any tracked file outside a changelog that names a command or variable wherry removed; the
# pattern is assembled from fragments so this file is scanned like every other.
set -euo pipefail
cd "$(dirname "$0")/.."
status=0

failed() {
	echo "FAIL: $*"
	status=1
}

# value prints the first match of the sed group in the regular expression $2 in the file $1.
value() { sed -n "s/$2/\\1/p" "$1" | head -n 1; }

# window checks that the drain_timeout $2, in whole seconds such as 20s, is at least 10 seconds below the stop window
# $3, in whole seconds such as 30 or 30s, for the example $1.
window() {
	local drain=${2%s} stop=${3%s}
	if ! [[ $drain =~ ^[0-9]+$ && $stop =~ ^[0-9]+$ ]]; then
		failed "$1: drain_timeout '$2' and stop window '$3' must be whole seconds"
	elif ((drain + 10 > stop)); then
		failed "$1: drain_timeout ${drain}s is not at least 10 seconds below the stop window, ${stop}s"
	else
		echo "$1: drain_timeout ${drain}s, stop window ${stop}s"
	fi
}

plugins=("$@")
[ $# -gt 0 ] || plugins=(nomad kubernetes systemd)
drain='^drain_timeout: *\([^ #]*\).*$'
for plugin in "${plugins[@]}"; do
	case $plugin in
	kubernetes)
		window examples/kubernetes "$(value examples/kubernetes/config.yaml "$drain")" \
			"$(value examples/kubernetes/receiver.yaml '^ *terminationGracePeriodSeconds: *\([^ #]*\).*$')"
		grep -x '        fsGroupChangePolicy: OnRootMismatch' examples/kubernetes/receiver.yaml >/dev/null ||
			failed "examples/kubernetes/receiver.yaml: an ownership pass on every mount opens signing.key to its group"
		;;
	nomad)
		window examples/nomad/wherry-receiver.nomad.hcl "$(value examples/nomad/config.yaml "$drain")" \
			"$(value examples/nomad/wherry-receiver.nomad.hcl '^ *kill_timeout *= *"\([^"]*\)".*$')"
		window examples/nomad/wherry-receiver-host.nomad.hcl \
			"$(value examples/nomad/wherry-receiver-host.nomad.hcl '^ *drain_timeout: *\([^ #]*\).*$')" \
			"$(value examples/nomad/wherry-receiver-host.nomad.hcl '^ *kill_timeout *= *"\([^"]*\)".*$')"
		for job in examples/nomad/wherry-receiver.nomad.hcl examples/nomad/wherry-receiver-host.nomad.hcl; do
			grep -F -x -e '        destination = "secrets/receiver.bundle"' "$job" >/dev/null &&
				grep -F -e '{{ with nomadVar "nomad/jobs/wherry-receiver" }}{{ .bundle }}{{ end }}' "$job" >/dev/null ||
				failed "$job: the bundle is not rendered from the Nomad variable nomad/jobs/wherry-receiver"
		done
		[ "$(value examples/nomad/config.yaml '^bundle: *\(.*\)$')" = /secrets/receiver.bundle ] ||
			failed "examples/nomad/config.yaml: bundle is not the rendered /secrets/receiver.bundle"
		;;
	systemd)
		window examples/systemd "$(value examples/systemd/config.yaml "$drain")" \
			"$(value examples/systemd/wherry-receiver.service '^TimeoutStopSec=\([^ #]*\).*$')"
		;;
	*) failed "no example for $plugin" ;;
	esac
done

if scripts=$(git ls-files 'examples/*.sh') && [ -n "$scripts" ]; then
	echo "$scripts"
	failed "a shell script is in the deployment path; use the platform's own tool"
fi

removed="wherry ""admin|WHERRY_""TRUST|wherry ""init\\b"
if hits=$(git grep -n -P "$removed" -- . ':!*CHANGELOG*'); then
	echo "$hits"
	failed "tracked files name a command or variable wherry removed"
fi
exit "$status"
