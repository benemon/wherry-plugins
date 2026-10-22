#!/usr/bin/env bash
# Creates the kind cluster wherry-e2e, replacing one of that name, and a Kafka under Strimzi in it. Needs docker, kind
# and kubectl. The Strimzi install file is pinned by SHA-256 and its operator and Kafka images by digest.
. "$(dirname "$0")/common.sh"
strimzi=1.3.0
strimzi_sha256=c58da61a66c20169d1c9c42736a9355b183ee17ac7779ab7875beaedbf972232
operator_image=quay.io/strimzi/operator@sha256:4deb564fa329afa4046c550a6f8f1a6d00be3b9297e556969acabe6647b6676f
kafka_image=quay.io/strimzi/kafka@sha256:1f189b1f0f32684302d8b018b1d21b411e2469a9c465fbdc18fa3599cb5bbdad

rm -rf "$work"
mkdir -p "$work/bin"
kind delete cluster --name "$cluster"
kind create cluster --name "$cluster" --config "$e2e/kind.yaml" --kubeconfig "$KUBECONFIG" --wait 5m

curl -fsSL -o "$work/strimzi.yaml" \
	"https://github.com/strimzi/strimzi-kafka-operator/releases/download/$strimzi/strimzi-cluster-operator-$strimzi.yaml"
echo "$strimzi_sha256  $work/strimzi.yaml" | sha256sum -c -
kubectl create namespace kafka
sed -e 's/namespace: myproject/namespace: kafka/' \
	-e "s|quay.io/strimzi/operator:$strimzi|$operator_image|" \
	-e "s|quay.io/strimzi/kafka:$strimzi-kafka-4.3.1|$kafka_image|" "$work/strimzi.yaml" |
	kubectl create -n kafka -f - >/dev/null
kubectl rollout status -n kafka deployment/strimzi-cluster-operator --timeout=5m
kubectl apply -n kafka -f "$e2e/kafka.yaml"
kubectl wait -n kafka kafka/wherry-kafka --for=condition=Ready --timeout=15m
kubectl wait -n kafka kafkauser --all --for=condition=Ready --timeout=5m
