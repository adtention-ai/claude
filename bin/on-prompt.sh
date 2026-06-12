#!/usr/bin/env bash
# Fires on UserPromptSubmit — the ONLY moment adtention mints a serve/impression.
# This is the activity gate: one billable impression per real human prompt.
#
# CRITICAL: must be silent. A UserPromptSubmit hook's stdout is injected into
# the prompt context, so we print NOTHING. Must also not block — fire-and-forget.

input=$(cat)
cwd=$(printf '%s' "$input" | jq -r '.cwd // empty');           [ -z "$cwd" ] && cwd="$PWD"
tp=$(printf '%s' "$input" | jq -r '.transcript_path // empty')

HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
( "$HERE/refresh.sh" "$cwd" "$tp" >/dev/null 2>&1 & )   # detached: refresh (and, in prod, the serve) happens off the critical path
exit 0
