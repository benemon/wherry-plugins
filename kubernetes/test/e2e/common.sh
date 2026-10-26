# shellcheck shell=bash disable=SC2034
# Sourced by each e2e step. The steps share their state in E2E_WORK, outside the checkout, so no build context holds it.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
e2e=$repo/kubernetes/test/e2e
work=${E2E_WORK:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wherry-e2e}
cluster=wherry-e2e
namespace=wherry
host_config=$work/host/config.yaml
export KUBECONFIG=$work/kubeconfig PATH=$work/bin:$PATH

# mask hides a secret in the GitHub Actions log; the steps never print one.
mask() { if [ "${GITHUB_ACTIONS:-}" = true ]; then echo "::add-mask::$1"; fi; }

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
