#!/usr/bin/env bash
# Collects the receiver's log and the cluster's state into $E2E_WORK/artifacts after a failed step. It reads no Secret.
. "$(dirname "$0")/common.sh"
set +e
out=$work/artifacts
mkdir -p "$out"
kubectl logs -n "$namespace" deployment/wherry-receiver >"$out/receiver.log" 2>&1
kubectl logs -n "$namespace" deployment/wherry-receiver --previous >"$out/receiver-previous.log" 2>&1
kubectl get nodes,namespaces,all,pvc,events -A -o wide >"$out/state.txt" 2>&1
kubectl get pods -n "$namespace" -o yaml >"$out/pods.yaml" 2>&1
kubectl describe pods -n "$namespace" >"$out/pods.txt" 2>&1
kubectl get kafka,kafkanodepool,kafkauser -n kafka -o yaml >"$out/kafka.yaml" 2>&1
kubectl logs -n kafka deployment/strimzi-cluster-operator >"$out/strimzi-operator.log" 2>&1
kubectl logs -n kafka -l strimzi.io/cluster=wherry-kafka --all-containers --prefix >"$out/kafka.log" 2>&1
exit 0
