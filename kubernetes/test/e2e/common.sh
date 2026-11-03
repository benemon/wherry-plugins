# shellcheck shell=bash disable=SC2034
# Sourced by each e2e step. The steps share their state in E2E_WORK, outside the checkout, so no build context holds it.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
e2e=$repo/kubernetes/test/e2e
work=${E2E_WORK:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wherry-e2e}
cluster=wherry-e2e
namespace=wherry
# The operator's working directory, and the host's own paths under $E2E_WORK/host.
operator=$work/operator
export KUBECONFIG=$work/kubeconfig PATH=$work/bin:$PATH XDG_CONFIG_HOME=$work/host/config XDG_STATE_HOME=$work/host/state
host_config=$XDG_CONFIG_HOME/wherry/config.yaml

# mask hides a secret in the GitHub Actions log; the steps never print one.
mask() { if [ "${GITHUB_ACTIONS:-}" = true ]; then echo "::add-mask::$1"; fi; }

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# wait_for runs a command every 5 seconds until it succeeds, for at most the given seconds.
wait_for() {
	local end=$((SECONDS + $1))
	shift
	until "$@"; do
		if ((SECONDS >= end)); then
			echo "timed out: $*" >&2
			return 1
		fi
		sleep 5
	done
}
