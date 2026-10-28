#!/bin/sh
# Copies the prompt to the output echo.txt and reports, in its outcome file, its UID and, for each input and credential
# file, its mode, owner and SHA-256. The prompt is E2E_PROMPT, by default /wherry/prompt.txt. A prompt holding
# "e2e: sleep" sleeps until stopped, and one holding "e2e: slow" replies after 90 seconds.
set -eu
prompt=${E2E_PROMPT:-/wherry/prompt.txt}
grep -q 'e2e: sleep' "$prompt" && exec sleep 3600
grep -q 'e2e: slow' "$prompt" && sleep 90
mkdir -p "$WHERRY_OUTPUT_DIR"
cp "$prompt" "$WHERRY_OUTPUT_DIR/echo.txt"
{
	echo "uid $(id -u)"
	for f in "$WHERRY_INPUT_DIR"/* "$WHERRY_CREDENTIALS_DIR" "$WHERRY_CREDENTIALS_DIR"/*; do
		[ -e "$f" ] || continue
		if [ -d "$f" ]; then echo "dir $(stat -c '%a %u' "$f")"; continue; fi
		echo "file $(basename "$f") $(stat -c '%a %u' "$f") $(sha256sum <"$f" | cut -d' ' -f1)"
	done
} >/tmp/reply
# The lines hold no quote or backslash, so joining them with \n makes the JSON string.
text=$(awk 'NR > 1 { printf "\\n" } { printf "%s", $0 }' /tmp/reply)
mkdir -p "$WHERRY_OUTPUT_DIR/.wherry"
printf '{"state":"TASK_STATE_COMPLETED","message":{"role":"ROLE_AGENT","parts":[{"text":"%s"}]}}\n' "$text" >"$WHERRY_OUTPUT_DIR/.wherry/outcome.json"
