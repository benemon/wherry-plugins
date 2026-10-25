#!/usr/bin/env bash
# Builds the receiver image with make image and the fake agent's image, loads both into kind, and applies the receiver
# with the README's commands into the namespace wherry, which enforces Pod Security restricted.
. "$(dirname "$0")/common.sh"
: "${WHERRY_VERSION:?}"
make -C "$repo" image CONTAINER=docker VERSION=e2e WHERRY_VERSION="$WHERRY_VERSION"
docker build -f "$e2e/agent/Containerfile" -t wherry-e2e-agent:e2e "$e2e/agent"
kind load docker-image --name "$cluster" wherry-receiver:e2e wherry-e2e-agent:e2e

kubectl create namespace "$namespace"
kubectl label namespace "$namespace" pod-security.kubernetes.io/enforce=restricted
NAMESPACE=$namespace IMAGE=wherry-receiver:e2e BOOTSTRAP=wherry-kafka-kafka-bootstrap.kafka.svc:9093 \
	BUNDLE=$work/receiver.bundle "$repo/examples/kubernetes/apply.sh"
kubectl rollout status -n "$namespace" deployment/wherry-receiver --timeout=5m
