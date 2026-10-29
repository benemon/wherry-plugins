#!/bin/sh
# Prints the release archives in the directory $1, each followed by the files it holds, with the version replaced by
# VERSION. ci.yml's release-build job compares it with .github/release-contents/<plugin>.txt, the archives the
# plugin's last release before goreleaser published. Directory entries are left out: goreleaser writes none, and tar
# creates the directories on extraction all the same.
set -eu
export LC_ALL=C
for archive in "$1"/*.tar.gz; do
	basename "$archive"
	tar -tzf "$archive" | grep -v '/$' | sort | sed 's/^/  /'
done | sed -E 's/_v[0-9][^_/]*_/_VERSION_/g'
