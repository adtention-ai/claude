#!/usr/bin/env bash
# adtention — SessionStart hook. Claude Code does NOT auto-apply a `statusLine` from a
# plugin's settings.json (only agent/subagentStatusLine), so we install ours into the
# user's settings here. Idempotent, and saves any prior statusLine for restore on uninstall.
set -uo pipefail

SETTINGS="$HOME/.claude/settings.json"
CACHE_DIR="${ADTENTION_CACHE:-$HOME/.claude/adtention}"
[ "$CACHE_DIR" = "$HOME/.claude/adtention" ] && [ -d "$HOME/.claude/adline" ] && [ ! -e "$CACHE_DIR" ] && mv "$HOME/.claude/adline" "$CACHE_DIR" 2>/dev/null
mkdir -p "$CACHE_DIR"

# show $0.00 from the very first render, before any serve (reinforces the earn hook)
[ -f "$CACHE_DIR/balance_display" ] || printf '⊕ $0.00' > "$CACHE_DIR/balance_display"

# the plugin's own statusline script (CLAUDE_PLUGIN_ROOT is set when run as a plugin hook)
ROOT="${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)}"
CMD="$ROOT/bin/statusline.sh"

command -v jq >/dev/null 2>&1 || exit 0      # jq required to edit settings safely; bail quietly
[ -f "$SETTINGS" ] || echo '{}' > "$SETTINGS"

current=$(jq -r '.statusLine.command // empty' "$SETTINGS" 2>/dev/null)
[ "$current" = "$CMD" ] && exit 0            # already installed → nothing to do

# capture any pre-existing statusLine so the render path can WRAP it (show theirs + ours),
# and save the full block once for restore on uninstall. Never wrap one of OUR OWN scripts
# (any adtention/adline statusline.sh) — that would recurse.
if [ -n "$current" ] && ! printf '%s' "$current" | grep -qE 'ad(tention|line).*statusline\.sh'; then
  printf '%s' "$current" > "$CACHE_DIR/wrapped_cmd"
  [ -f "$CACHE_DIR/prev_statusline.json" ] || jq '.statusLine' "$SETTINGS" > "$CACHE_DIR/prev_statusline.json" 2>/dev/null || true
else
  rm -f "$CACHE_DIR/wrapped_cmd"             # nothing to wrap → normal mode
fi

tmp=$(mktemp)
if jq --arg cmd "$CMD" '.statusLine = {type:"command", command:$cmd, refreshInterval:10}' "$SETTINGS" > "$tmp" 2>/dev/null; then
  mv "$tmp" "$SETTINGS"
else
  rm -f "$tmp"
fi
exit 0
