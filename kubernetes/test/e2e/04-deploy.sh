#!/usr/bin/env bash
# Loads the receiver images, the checkout's and the previous release's, and the fake agent's image into kind, creates
# the runs claim's volume and the state claim's, and deploys the receiver with the README's steps into the namespace
# wherry, which enforces Pod Security restricted: a copy of examples/kubernetes holding the receiver's bundle and the
# Strimzi cluster CA, with ca_file, the fsGroup patch and the image set, applied with kubectl apply -k. The state
# volume's root is root's, so the kubelet's ownership pass gives it the fsGroup on its first mount. The receiver is then
# restarted, and must start with its signing key still 0600.
. "$(dirname "$0")/common.sh"
kind load docker-image --name "$cluster" wherry-receiver:e2e wherry-receiver:previous wherry-e2e-agent:e2e

# hostPath takes no fsGroup, so the receiver's UID owns the directory.
docker exec "$cluster-control-plane" install -d -o 1001 -g 0 -m 0700 /var/lib/wherry-runs
docker exec "$cluster-control-plane" install -d -o 0 -g 0 -m 0755 /var/lib/wherry-state
kubectl apply -f "$e2e/runs.yaml" -f "$e2e/state.yaml"
kubectl create namespace "$namespace"
kubectl label namespace "$namespace" pod-security.kubernetes.io/enforce=restricted

cp -R "$repo/examples/kubernetes" "$deploy"
cp "$operator/bundles/receivers/kind-1.bundle" "$deploy/receiver.bundle"
cp "$work/ca.crt" "$deploy/kafka-ca.crt"
sed -i 's|^# ca_file: |ca_file: |' "$deploy/config.yaml"
sed -i -e 's|newName: registry.example.com/wherry-receiver, newTag: <version>}|newName: wherry-receiver, newTag: e2e}|' \
	-e 's|^  # - {name: wherry-receiver-kafka-ca,|  - {name: wherry-receiver-kafka-ca,|' \
	-e 's|^  # - path: fsgroup.yaml$|  - path: fsgroup.yaml|' "$deploy/kustomization.yaml"
grep -x 'ca_file: /etc/wherry/kafka-ca/ca.crt' "$deploy/config.yaml" >/dev/null || fail "config.yaml names no ca_file"
for line in 'newTag: e2e}' '  - {name: wherry-receiver-kafka-ca,' '  - path: fsgroup.yaml'; do
	grep -F -e "$line" "$deploy/kustomization.yaml" >/dev/null || fail "kustomization.yaml lacks '$line'"
done
kubectl apply -k "$deploy"
kubectl rollout status -n "$namespace" deployment/wherry-receiver --timeout=5m

receiver() { kubectl exec -n "$namespace" deployment/wherry-receiver -c receiver -- "$@"; }
started() {
	kubectl logs -n "$namespace" deployment/wherry-receiver -c receiver 2>/dev/null |
		grep -F 'plugin kubernetes: version e2e ' >/dev/null
}
wait_for 120 started
receiver test -f /var/lib/wherry/state/signing.key || fail "the receiver wrote no signing key"
# The first mount's ownership pass gave the volume's root the fsGroup and setgid.
mode=$(receiver stat -c '%g %A' /var/lib/wherry)
[ "$mode" = "1001 drwxrwsr-x" ] || fail "the state volume's root is '$mode'; want the ownership pass's '1001 drwxrwsr-x'"
kubectl rollout restart -n "$namespace" deployment/wherry-receiver
kubectl rollout status -n "$namespace" deployment/wherry-receiver --timeout=5m
wait_for 120 started
mode=$(receiver stat -c %a /var/lib/wherry/state/signing.key)
[ "$mode" = 600 ] || fail "signing.key is $mode after a restart; want 600"
echo "PASS"
