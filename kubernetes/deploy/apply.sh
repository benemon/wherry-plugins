#!/usr/bin/env bash
# Applies the receiver to the existing namespace NAMESPACE: the Secret wherry-receiver-bundle from the receiver bundle
# BUNDLE with its broker replaced by the in-cluster bootstrap address BOOTSTRAP, then receiver.yaml with the image IMAGE.
# The bundle passes through memory and is never written to disk. Run it again to change either, and restart the
# Deployment for a new bundle.
set -euo pipefail
: "${NAMESPACE:?}" "${IMAGE:?}" "${BOOTSTRAP:?}" "${BUNDLE:?}"
secret=$(sed "s|^broker: .*|broker: $BOOTSTRAP|" "$BUNDLE" |
  kubectl create secret generic wherry-receiver-bundle -n "$NAMESPACE" --from-file=bundle.yaml=/dev/stdin \
    --dry-run=client -o yaml)
printf '%s\n' "$secret" | kubectl apply --server-side -n "$NAMESPACE" -f -
sed -e "s|namespace: wherry}|namespace: $NAMESPACE}|" -e "s|image: wherry-receiver\$|image: $IMAGE|" \
  "$(dirname "$0")/receiver.yaml" |
  kubectl apply --server-side -n "$NAMESPACE" -f -
