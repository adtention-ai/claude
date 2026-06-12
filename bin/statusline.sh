#!/usr/bin/env bash
# adtention — render path. Fast: reads cached ad/balance + prints. Never hits the network.
#
# WRAP MODE (user already had a statusLine): run theirs, then append our slot — inline if
#   it fits the terminal width, else on a second line. Their setup is preserved.
# NORMAL MODE (no prior statusLine): render our own status segments + slot, width-aware.
#
# No ad available → the ad part is simply omitted (earnings still shown). No placeholder.

CACHE_DIR="${ADTENTION_CACHE:-$HOME/.claude/adtention}"
[ "$CACHE_DIR" = "$HOME/.claude/adtention" ] && [ -d "$HOME/.claude/adline" ] && [ ! -e "$CACHE_DIR" ] && mv "$HOME/.claude/adline" "$CACHE_DIR" 2>/dev/null
input=$(cat)

# cached slot parts — written by the refresher, off the render path. Either may be empty.
ad=$(cat "$CACHE_DIR/current_ad.txt" 2>/dev/null)
balseg=$(cat "$CACHE_DIR/balance_display" 2>/dev/null)

cols=${COLUMNS:-80}; case "$cols" in (*[!0-9]*|'') cols=80;; esac
vislen() { printf '%s' "$1" | sed -E 's/\x1b\[[0-9;]*m//g' | awk '{print length; exit}'; }

# build our slot from whichever parts exist (green balance, cyan ad). Both protected.
slot=""; slot_w=0
if [ -n "$balseg" ]; then
  slot="$(printf '\033[1;32m%s\033[0m' "$balseg")"; slot_w=${#balseg}
fi
if [ -n "$ad" ]; then
  if [ -n "$slot" ]; then slot="$slot  $(printf '\033[36m%s\033[0m' "$ad")"; slot_w=$(( slot_w + 2 + ${#ad} ))
  else slot="$(printf '\033[36m%s\033[0m' "$ad")"; slot_w=${#ad}; fi
fi
gap=0; [ -n "$slot" ] && gap=2   # only reserve a gap if there's a slot to show

wrapped=$(cat "$CACHE_DIR/wrapped_cmd" 2>/dev/null)
if [ -n "$wrapped" ]; then
  # --- WRAP MODE: render the user's own status line, then our slot (if any) ---
  their=$(printf '%s' "$input" | eval "$wrapped" 2>/dev/null)
  if [ -z "$slot" ]; then printf '%s' "$their"; exit 0; fi
  nl=$(printf '%s' "$their" | wc -l | tr -d ' ')
  if [ "$nl" -eq 0 ] && [ $(( $(vislen "$their") + slot_w + 2 )) -le "$cols" ]; then
    printf '%s  %s' "$their" "$slot"          # inline
  else
    printf '%s\n%s' "$their" "$slot"          # second line
  fi
else
  # --- NORMAL MODE: our own status segments (width-aware shed), then slot (if any) ---
  IFS=$'\t' read -r model ctx d7 <<EOF
$(printf '%s' "$input" | jq -r '[ (.model.display_name // "?"), (.context_window.used_percentage // null), (.rate_limits.seven_day.used_percentage // null) ] | map(if type=="number" then (round|tostring) else (. // "") end) | @tsv')
EOF
  model="${model%% (*}"
  vals=("$model" "${ctx:+context ${ctx}%}" "${d7:+limit ${d7}%}")
  present=(1 1 1)
  assemble() {
    local out="" i
    for i in 0 1 2; do
      [ "${present[$i]}" = 1 ] || continue
      [ -n "${vals[$i]}" ] || continue
      if [ -n "$out" ]; then out="$out · ${vals[$i]}"; else out="${vals[$i]}"; fi
    done
    printf '%s' "$out"
  }
  budget=$(( cols - slot_w - gap ))
  status="$(assemble)"
  for di in 0 1; do [ ${#status} -le "$budget" ] && break; present[$di]=0; status="$(assemble)"; done
  if [ -n "$slot" ]; then printf '\033[2m%s\033[0m  %s' "$status" "$slot"
  else printf '\033[2m%s\033[0m' "$status"; fi
fi
