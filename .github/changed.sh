#!/usr/bin/env bash
# Writes changed=true to $GITHUB_OUTPUT when a file whose path matches the extended regular expression $2 changed
# between the commit $1 and HEAD, or when $1 is not a known commit, as on a new branch. A job gates its later steps on
# it, so a job with nothing to check reports success.
set -euo pipefail
changed=true
if [ -n "$1" ] && git cat-file -e "$1^{commit}" 2>/dev/null; then
	git diff --name-only "$1" HEAD | grep -E "$2" >/dev/null || changed=false
fi
echo "changed=$changed" >>"$GITHUB_OUTPUT"
