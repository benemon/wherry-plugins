#!/usr/bin/env bash
# Loads the receiver image and the fake agent's image into kind, creates the runs claim's volume, and applies the
# receiver with the README's commands into the namespace wherry, which enforces Pod Security restricted.
. "$(dirname "$0")/common.sh"
kind load docker-image --name "$cluster" wherry-receiver:e2e wherry-e2e-agent:e2e

# hostPath takes no fsGroup, so the receiver's UID owns the directory.
docker exec "$cluster-control-plane" install -d -o 1001 -g 0 -m 0700 /var/lib/wherry-runs
kubectl apply -f "$e2e/runs.yaml"
kubectl create namespace "$namespace"
kubectl label namespace "$namespace" pod-security.kubernetes.io/enforce=restricted
NAMESPACE=$namespace IMAGE=wherry-receiver:e2e BOOTSTRAP=wherry-kafka-kafka-bootstrap.kafka.svc:9093 \
	BUNDLE=$work/receiver.bundle "$repo/examples/kubernetes/apply.sh"
kubectl rollout status -n "$namespace" deployment/wherry-receiver --timeout=5m
