#!/usr/bin/env bash
# Runs the plugin's conformance suite, direct, over the link and under the receiver's Role, in a pod in the kind cluster,
# since the runs claim is a hostPath inside the kind node, which the runner cannot see. It provisions what the suite
# needs and nothing of the receiver: the runs claim's directory and volume, the namespace wherry under Pod Security
# restricted, and from examples/kubernetes/receiver.yaml the service account, its Role and binding and the runs claim.
# The pod, conformance.yaml, runs the suite as UID 1001 with that service account's token; its pending profile asks for
# more CPU than the node has. 04-deploy.sh applies the same objects again. Needs docker, kind, kubectl, go and jq.
. "$(dirname "$0")/common.sh"
pod=wherry-conformance

image=$(sed -n 's|^ *image: docker.io/library/golang:\([^@]*\)@.*|\1|p' "$e2e/conformance.yaml")
go=$(go mod edit -json "$repo/kubernetes/go.mod" | jq -r .Go)
[ "$image" = "$go" ] || fail "conformance.yaml runs Go $image; kubernetes/go.mod names go $go"

docker exec "$cluster-control-plane" install -d -o 1001 -g 0 -m 0700 /var/lib/wherry-runs
kubectl apply -f "$e2e/runs.yaml"
kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f -
kubectl label --overwrite namespace "$namespace" pod-security.kubernetes.io/enforce=restricted
rbac=$work/conformance-rbac
mkdir -p "$rbac"
cp "$repo/examples/kubernetes/receiver.yaml" "$rbac/"
cat >"$rbac/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: $namespace
resources: [receiver.yaml]
patches:
  - patch: '{"\$patch": "delete", "apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "wherry-receiver"}}'
  - patch: '{"\$patch": "delete", "apiVersion": "v1", "kind": "Service", "metadata": {"name": "wherry-receiver-metrics"}}'
  - patch: '{"\$patch": "delete", "apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"name": "wherry-receiver-state"}}'
EOF
kubectl apply -k "$rbac"

kubectl delete pod -n "$namespace" "$pod" --ignore-not-found --wait
kubectl apply -n "$namespace" -f "$e2e/conformance.yaml"
kubectl wait -n "$namespace" "pod/$pod" --for=condition=Ready --timeout=5m
suite() { kubectl exec -i -n "$namespace" "$pod" -c suite -- "$@"; }
suite mkdir -p /work/tmp /work/src
# The module's tracked files alone, without the e2e scripts or a build output.
git -C "$repo/kubernetes" ls-files -z -- . ':!test' | tar -C "$repo/kubernetes" --null -T - -cf - | suite tar -C /work/src -xf -
suite sh -c 'cd /work/src && go version && go mod download'
code=0
suite sh -c 'cd /work/src && go test -count=1 -p 1 -timeout 30m -v -run "^(TestConformance|TestConformanceOverTheLink|TestRoleSuffices)\$" .' |
	tee "$work/conformance.log" || code=$?
kubectl delete pod -n "$namespace" "$pod" --wait=false
[ "$code" = 0 ] || fail "the conformance suite failed; see $work/conformance.log"
# A skipped suite passes too, so each must have run.
for test in TestConformance TestConformanceOverTheLink TestRoleSuffices; do
	grep -x -- "--- PASS: $test ([0-9.]*s)" "$work/conformance.log" >/dev/null || fail "$test did not run and pass"
	grep -E -- "^    --- PASS: $test/Pending " "$work/conformance.log" >/dev/null || fail "$test ran no pending profile"
done
echo "PASS"
