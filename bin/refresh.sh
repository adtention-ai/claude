#!/usr/bin/env bash
# adtention — background refresher. NOT on the render path; may be slow.
# Classifies LOCALLY, then calls the server: the single /v1/serve request IS the
# impression. The server is the source of truth for ad selection + the ledger.
# Only a category tag (never code/prompts) leaves the box.
#
# Usage: refresh.sh <cwd> [transcript_path]
# Env:   ADTENTION_API   (default = production Worker)
#        ADTENTION_CACHE (default ~/.claude/adtention)
set -uo pipefail

CACHE_DIR="${ADTENTION_CACHE:-$HOME/.claude/adtention}"
# one-time migration from the old default cache dir (pre-rename), preserving identity/balance
[ "$CACHE_DIR" = "$HOME/.claude/adtention" ] && [ -d "$HOME/.claude/adline" ] && [ ! -e "$CACHE_DIR" ] && mv "$HOME/.claude/adline" "$CACHE_DIR" 2>/dev/null
API="${ADTENTION_API:-https://adline-server.len-525.workers.dev}"
mkdir -p "$CACHE_DIR"

# portable mtime (BSD stat -f, GNU stat -c)
mtime() { stat -f %m "$1" 2>/dev/null || stat -c %Y "$1" 2>/dev/null || echo 0; }

# single-flight lock, with stale-lock recovery (a kill -9 mid-refresh would otherwise
# leak the lock and bail every future refresh forever)
LOCK="$CACHE_DIR/refresh.lock"
[ -d "$LOCK" ] && [ $(( $(date +%s) - $(mtime "$LOCK") )) -ge 60 ] && rmdir "$LOCK" 2>/dev/null
if ! mkdir "$LOCK" 2>/dev/null; then exit 0; fi
trap 'rmdir "$LOCK" 2>/dev/null' EXIT

cwd="${1:-$PWD}"
transcript="${2:-}"

# --- folder-type classifier (fallback signal) ---
classify_folder() {
  local d="$1"
  if [ -e "$d/foundry.toml" ] || compgen -G "$d/*.sol" >/dev/null || compgen -G "$d/hardhat.config.*" >/dev/null; then echo web3; return; fi
  if [ -e "$d/Dockerfile" ] || compgen -G "$d/*.tf" >/dev/null; then echo devops; return; fi
  if [ -e "$d/package.json" ]; then echo web; return; fi
  if [ -e "$d/requirements.txt" ] || compgen -G "$d/*.py" >/dev/null; then echo data; return; fi
  if [ -e "$d/Cargo.toml" ] || [ -e "$d/go.mod" ]; then echo systems; return; fi
  echo general
}

# --- conversation-topic classifier: scans recent transcript locally (crude keyword
#     scoring; prod uses a small model). Only the winning TAG is ever sent. ---
classify_topic() {
  local tp="$1"
  [ -f "$tp" ] || return 1
  local text; text=$(tail -n 400 "$tp" 2>/dev/null | tr '[:upper:]' '[:lower:]')
  [ -z "$text" ] && return 1
  hits() { printf '%s' "$text" | grep -oE "$1" 2>/dev/null | grep -c . ; }
  local s_web3 s_web s_devops s_data s_systems
  s_web3=$(hits 'solidity|ethereum|web3|smart contract|defi|onchain|blockchain|wallet|stablecoin|crypto|erc-?20')
  s_web=$(hits 'react|tailwind|next\.js|frontend|vite|jsx|tsx|css|component')
  s_devops=$(hits 'docker|kubernetes|terraform|kubectl|nginx|ci/cd|pipeline|deployment')
  s_data=$(hits 'dataset|training data|pandas|embedding|inference|fine-tune|gpu|machine learning')
  s_systems=$(hits 'goroutine|borrow checker|mutex|concurrency|memory safety|rustc')
  local best
  best=$(printf '%s web3\n%s web\n%s devops\n%s data\n%s systems\n' \
           "$s_web3" "$s_web" "$s_devops" "$s_data" "$s_systems" | sort -rn | head -1)
  local n="${best%% *}" c="${best##* }"
  if [ "${n:-0}" -ge 3 ]; then echo "$c"; return 0; fi
  return 1
}

# topic wins when confident; else folder; else general
category=""
src="folder"
if [ -n "$transcript" ] && category="$(classify_topic "$transcript")" && [ -n "$category" ]; then
  src="topic"
else
  category="$(classify_folder "$cwd")"
fi

# --- identity: register once, cache publisher_id + secret (chmod 600) ---
ID_FILE="$CACHE_DIR/identity.json"
pubid=""
[ -f "$ID_FILE" ] && pubid=$(jq -r '.publisher_id // empty' "$ID_FILE" 2>/dev/null)
if [ -z "$pubid" ]; then
  reg=$(curl -s -m 5 -X POST "$API/v1/register" 2>/dev/null || true)
  if [ -n "$reg" ]; then
    printf '%s' "$reg" > "$ID_FILE"; chmod 600 "$ID_FILE" 2>/dev/null || true
    pubid=$(printf '%s' "$reg" | jq -r '.publisher_id // empty' 2>/dev/null)
  fi
fi
[ -z "$pubid" ] && exit 0   # server unreachable & no identity → leave cache as-is

# --- dwell / frequency cap: don't re-serve within MIN_DWELL seconds. Guarantees an ad
#     stays visible long enough, and (shared across terminals via the common cache dir)
#     means rapid prompts — or many open terminals — can't churn or game impressions. ---
MIN_DWELL=15
LAST="$CACHE_DIR/last_serve"
nowsec=$(date +%s)
last=$(cat "$LAST" 2>/dev/null || echo 0)
if [ $(( nowsec - last )) -lt "$MIN_DWELL" ]; then exit 0; fi   # keep current ad
printf '%s' "$nowsec" > "$LAST"

# --- serve: the ONE network call. this request IS the impression. ---
serve_call() {
  curl -s -m 5 -X POST "$API/v1/serve" -H 'content-type: application/json' \
    -d "{\"publisher_id\":\"$pubid\",\"category\":\"$category\",\"nonce\":\"$1\"}" 2>/dev/null || true
}
nonce="$(date +%s)-${RANDOM}"
resp=$(serve_call "$nonce")

# self-heal: if the server doesn't know this publisher (e.g. DB reset / fresh server),
# re-register once and retry — otherwise a stale identity bricks the client forever.
if printf '%s' "$resp" | grep -q 'unknown_publisher'; then
  reg=$(curl -s -m 5 -X POST "$API/v1/register" 2>/dev/null || true)
  if [ -n "$reg" ]; then
    printf '%s' "$reg" > "$ID_FILE"; chmod 600 "$ID_FILE" 2>/dev/null || true
    pubid=$(printf '%s' "$reg" | jq -r '.publisher_id // empty' 2>/dev/null)
    resp=$(serve_call "${nonce}-r")
  fi
fi
[ -z "$resp" ] && exit 0     # server UNREACHABLE → keep last cached ad (graceful)

adtext=$(printf '%s' "$resp" | jq -r '.text // empty' 2>/dev/null)
bal=$(printf '%s' "$resp"   | jq -r '.balance_usd // empty' 2>/dev/null)

# record balance whenever the server reported one
if [ -n "$bal" ]; then
  printf '%s' "$bal" > "$CACHE_DIR/balance"
  awk -v b="$bal" 'BEGIN{printf "⊕ $%.2f", b}' > "$CACHE_DIR/balance_display"
fi

# Server RESPONDED but no ad (e.g. no_inventory): clear the slot → show nothing, not a
# stale ad. (Unreachable server was already handled above by keeping the last ad.)
if [ -z "$adtext" ]; then
  : > "$CACHE_DIR/current_ad.txt"
  exit 0
fi

printf '%s' "$adtext"   > "$CACHE_DIR/current_ad.txt"
printf '%s' "$category" > "$CACHE_DIR/category.txt"
printf '%s' "$src"      > "$CACHE_DIR/source.txt"
# local mirror only — the server's ledger is authoritative
printf '%s\t%s\t%s\t%s\n' "$(date +%s)" "$src" "$category" "$adtext" >> "$CACHE_DIR/impressions.log"
