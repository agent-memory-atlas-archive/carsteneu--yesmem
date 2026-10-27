---
name: telegram
description: "Bidirectional Telegram messaging via Bot API — send messages, poll for updates, reply via headless agent."
version: 604
tags: [telegram, bot, messaging]
requires: [store]
scope: user
auto_active: true
---

## Purpose

Bidirectional Telegram messaging via Bot API — send messages, poll for updates, reply via headless agent.

In-band slash commands (case-insensitive, processed before LLM invocation):

| Command | Effect |
|---|---|
| `/sessions` | List all registered sessions, active marked with `(*)` |
| `/use <name>` | Switch to session `<name>` (creates it with empty session_id if new) |
| `/use <name> <session_id>` | Switch and explicitly set a session_id (resume existing opencode session) |
| `/status` | Show active session's name, session_id, last_used_at |
| `/model` | Show active model (session-specific or global default) |
| `/model <provider/model>` | Set model for active session (e.g. `deepseek/deepseek-v4-pro`) |
| `/model default` | Clear session-specific model (alias: `clear`, `reset`) → falls back to global |
| `/models` | List configured `provider/model` entries (from `YESMEM_OPENCODE_MODELS`, which the daemon fills for a sandboxed run; falls back to `~/.config/opencode/opencode.json`) |

**Active session** is tracked in `cap_telegram__sessions` (column `is_default=1`). Falls back to legacy `reply_session` config key when the sessions table is empty (backwards compatibility).

**Model resolution** per reply: active session's `model` column → global `reply_model` config → Bash default `claude-sonnet-4-6`.

Note: the `cap_telegram__sessions` table is created by `yesmem setup` or a one-shot `cap_store {action:"create_table", capability:"telegram", table:"sessions", columns:[...]}` call — the `## Database` SQL block below is NOT executed automatically by the CapsDirWatcher sync (Learning #55757).

## Scripts

### telegram_send
kind: tool
schema: {"type":"object","properties":{"text":{"type":"string","description":"Message text"},"chat_id":{"type":"string","description":"Optional chat id override"},"reply_to":{"type":"integer","description":"Optional reply-to message id"}},"required":["text"]}
sandbox: standard
allowed_hosts: api.telegram.org

```bash
: "${YESMEM_JOB_DIR:=/tmp}"
mkdir -p "$YESMEM_JOB_DIR" 2>/dev/null || YESMEM_JOB_DIR=/tmp
# keep the log bounded: the job dir survives the run. Truncating after
# the redirect below would leave a sparse file, so it happens before it.
if [ -f "$YESMEM_JOB_DIR/tsend.log" ] && [ "$(wc -c <"$YESMEM_JOB_DIR/tsend.log")" -gt 1048576 ]; then : >"$YESMEM_JOB_DIR/tsend.log"; fi
exec 2>>"$YESMEM_JOB_DIR/tsend.log"
TOKEN=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["bot_token"],"limit":1}' | yesmem json -r '.rows[0].value')
CHAT_ID=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["chat_id"],"limit":1}' | yesmem json -r '.rows[0].value')
# A bare curl answers a refused delivery with exit 0 and writes the API's error
# body to stdout, so this tool used to hand back an error as its result and the
# caller believed the message went out (YM-368). The answer is read instead of
# made fatal: Telegram names the reason in that body ("chat not found", "can't
# parse entities"), and -f would drop the body and leave a bare exit code, which
# is exactly the diagnosis the caller needs. The .ok check covers a refusal that
# arrives with 200 and a body that is not JSON at all. Either way the send ends
# non-zero, and the daemon turns that into a failed cap execution: the only
# signal the caller can act on. The assignment takes the status of the
# substitution, so "||" keeps it in STATUS instead of ending the run under the
# prepended set -e before the check below could read it.
STATUS=0
# --data-urlencode for the text: -d sends the value as it is, so an "&" in it
# started another form field (measured 20.10.2026, a text of
# "hi&chat_id=666&parse_mode=HTML" arrived as three fields). The reply bodies
# already encode their text the same way.
RESPONSE=$(curl -4 -sS -m 10 "https://api.telegram.org/bot${TOKEN}/sendMessage" -d "chat_id=${CHAT_ID}" --data-urlencode "text=${TEXT}" -d "parse_mode=Markdown") || STATUS=$?
if [ "$STATUS" -ne 0 ] || [ "$(echo "$RESPONSE" | yesmem json -r '.ok // false')" != "true" ]; then
  # The reason is a sentence the API wrote, and this line is a log line: control
  # characters would let it add one of its own. The same treatment as the log in
  # the poll and reply bodies, including the C1 block, inlined because this is
  # the only foreign value.
  REASON=$(echo "$RESPONSE" | yesmem json -r '.description // empty') || REASON=""
  REASON=${REASON//[$'\001'-$'\037'$'\177']/ }
  REASON=${REASON//$'\302'[$'\200'-$'\237']/ }
  FAILED="sendMessage failed status=${STATUS}"
  if [ -n "$REASON" ]; then FAILED="${FAILED}: ${REASON:0:200}"; fi
  printf '[%s] send: %s\n' "$(date -Is)" "$FAILED" >> "$YESMEM_JOB_DIR/tsend.log"
  # stderr is the log file here, so the reason has to reach the caller on stdout.
  printf '%s\n' "$FAILED"
  exit 1
fi
printf '%s\n' "$RESPONSE"
```

### telegram_poll
kind: handler
sandbox: standard
allowed_hosts: api.telegram.org

```bash
: "${YESMEM_JOB_DIR:=/tmp}"
mkdir -p "$YESMEM_JOB_DIR" 2>/dev/null || YESMEM_JOB_DIR=/tmp
# keep the log bounded: the job dir survives the run. Truncating after
# the redirect below would leave a sparse file, so it happens before it.
if [ -f "$YESMEM_JOB_DIR/tpoll.log" ] && [ "$(wc -c <"$YESMEM_JOB_DIR/tpoll.log")" -gt 1048576 ]; then : >"$YESMEM_JOB_DIR/tpoll.log"; fi
exec 2>>"$YESMEM_JOB_DIR/tpoll.log"
printf '[%s] poll start\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/tpoll.log"
# The sender name in the log below comes from Telegram, so it is foreign text: a
# newline in it would write a line the reader takes for ours (YM-368). Every
# control character becomes a space, which keeps the fragments of a message
# apart and leaves no way to forge or rewrite a line. Besides C0 and DEL that
# covers the C1 block, which jq emits as the two-byte form C2 80..C2 9F and of
# which U+009B is the one-byte form of CSI. Bash does this itself: a sandboxed
# run has only the binaries the cap declares, and a shell builtin cannot go
# missing. The cut counts characters only when the run has a UTF-8 locale;
# otherwise it counts bytes and can end the excerpt inside a multi-byte
# character, which stays cosmetic.
log_text() {
  local s
  s=${1//[$'\001'-$'\037'$'\177']/ }
  s=${s//$'\302'[$'\200'-$'\237']/ }
  printf '%s' "${s:0:${2:-80}}"
}

TOKEN=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["bot_token"],"limit":1}' | yesmem json -r '.rows[0].value')
if [ -z "$TOKEN" ]; then printf '[%s] poll: no bot_token\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/tpoll.log"; exit 0; fi

OFFSET=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["offset"],"limit":1}' | yesmem json -r '.rows[0].value // "0"')

# The assignment takes the status of the substitution: under the prepended
# set -e a transport failure ended the run here, before the check on the next
# line could read $?. The run ended non-zero, so the job counted as failed, and
# the branch below (its own log line, exit 0) was dead code; all the log had was
# curl's own stderr line (YM-368, the same sink YM-337 fixed in
# telegram_reply). The "||" keeps the failure in RET, so a network hiccup stays
# a logged step and the next run retries.
RET=0
RESPONSE=$(curl -4 -sS -m 14 "https://api.telegram.org/bot${TOKEN}/getUpdates?offset=${OFFSET}&timeout=12") || RET=$?
if [ "$RET" -ne 0 ]; then printf '[%s] poll: curl failed exit=%s\n' "$(date -Is)" "$RET" >> "$YESMEM_JOB_DIR/tpoll.log"; exit 0; fi

COUNT=$(echo "$RESPONSE" | yesmem json '.result | length') || COUNT=""
# An unreadable body is a distinct answer, not "no messages": reporting a broken
# parse as an idle bot would hide a permanently failing poll (consequence review of
# YM-394). Both are a logged step with exit 0, as everywhere else in this file.
if [ -z "$COUNT" ]; then
  printf '[%s] poll: response unreadable\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/tpoll.log"
  exit 0
fi
if [ "$COUNT" = "0" ] || [ "$COUNT" = "null" ]; then
  printf '[%s] poll: no messages\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/tpoll.log"
  exit 0
fi

printf '[%s] poll: %s updates\n' "$(date -Is)" "$COUNT" >> "$YESMEM_JOB_DIR/tpoll.log"
MAX_ID=$OFFSET
N=0
for i in $(seq 0 $((COUNT - 1))); do
  N=$((N + 1))
  if [ $N -gt 50 ]; then printf '[%s] poll: LIMIT 50 reached, deferring remaining\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/tpoll.log"; break; fi
  # Every read below carries its own "||": the status of an assignment is the
  # status of the substitution, so under the prepended set -e one unreadable field
  # ended the whole loop, the offset was never written, and the batch came again on
  # the next run (YM-394, the same sink as the count above).
  UPDATE=$(echo "$RESPONSE" | yesmem json ".result[$i]") || UPDATE=""
  UPD_ID=$(echo "$UPDATE" | yesmem json -r '.update_id') || UPD_ID=""
  PAYLOAD=$(echo "$UPDATE" | yesmem json '{capability:"telegram",action:"upsert",table:"updates",data:{telegram_id:.update_id,chat_id:.message.chat.id,sender:(.message.from.first_name // "unknown"),text:(.message.text // ""),direction:"in",processed:0,date:.message.date}}') || PAYLOAD=""
  # break, not continue: MAX_ID must not walk past an update that was not stored,
  # otherwise getUpdates(offset=MAX_ID) never returns it again and the message is
  # lost for good. Stopping the batch keeps it for the next run, whose upserts are
  # idempotent (consequence review of YM-394).
  if [ -z "$PAYLOAD" ]; then printf '[%s] poll: update %s unreadable, batch stopped\n' "$(date -Is)" "$i" >> "$YESMEM_JOB_DIR/tpoll.log"; break; fi
  echo "$PAYLOAD" | while IFS= read -r p; do store "$p" > /dev/null; done \
    || { printf '[%s] poll: update %s not stored\n' "$(date -Is)" "$i" >> "$YESMEM_JOB_DIR/tpoll.log"; break; }
  # 10# so a leading zero is decimal, never octal, and a non-numeric value cannot
  # reach the arithmetic at all (security review of YM-394).
  if [ -n "$UPD_ID" ] && [ "$UPD_ID" -ge "$MAX_ID" ] 2>/dev/null; then MAX_ID=$((10#$UPD_ID + 1)); fi
  # The status of this assignment is the status of the substitution, so it
  # carries its own "||": without it a failing read ends the loop under set -e,
  # the offset is never written and the same updates come again (YM-368).
  SENDER=$(echo "$UPDATE" | yesmem json -r '(.message.from.first_name // "?")') || SENDER="?"
  printf '[%s] poll: stored update_id=%s from %s\n' "$(date -Is)" "$UPD_ID" "$(log_text "$SENDER")" >> "$YESMEM_JOB_DIR/tpoll.log"
done

OFFSET_PAYLOAD=$(yesmem json -n --arg key "offset" --arg value "$MAX_ID" '{capability:"telegram",action:"upsert",table:"config",data:{id:12,key:$key,value:$value}}') || OFFSET_PAYLOAD=""
# The same sink one line after the loop this ticket closes: without the guard the
# run ends here and the offset is never written, so the batch comes again (cold
# review of YM-394).
if [ -n "$OFFSET_PAYLOAD" ]; then
  echo "$OFFSET_PAYLOAD" | while IFS= read -r p; do store "$p" > /dev/null; done \
    || printf '[%s] poll: offset write failed\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/tpoll.log"
else
  printf '[%s] poll: offset not stored\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/tpoll.log"
fi
printf '[%s] poll: done, offset=%s\n' "$(date -Is)" "$MAX_ID" >> "$YESMEM_JOB_DIR/tpoll.log"
```

### telegram_reply
kind: handler
sandbox: standard
allowed_hosts: api.telegram.org

```bash
: "${YESMEM_JOB_DIR:=/tmp}"
mkdir -p "$YESMEM_JOB_DIR" 2>/dev/null || YESMEM_JOB_DIR=/tmp
# keep the log bounded: the job dir survives the run. Truncating after
# the redirect below would leave a sparse file, so it happens before it.
if [ -f "$YESMEM_JOB_DIR/treply.log" ] && [ "$(wc -c <"$YESMEM_JOB_DIR/treply.log")" -gt 1048576 ]; then : >"$YESMEM_JOB_DIR/treply.log"; fi
exec 2>>"$YESMEM_JOB_DIR/treply.log"
printf '[%s] reply: check\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
# The claimed message text, its sender and the model's answer are all foreign
# text: a newline in one of them would write a log line the reader takes for
# ours (YM-368). Every control character becomes a space, which keeps the
# fragments of a message apart and leaves no way to forge or rewrite a line.
# Besides C0 and DEL that covers the C1 block, which jq emits as the two-byte
# form C2 80..C2 9F and of which U+009B is the one-byte form of CSI. Bash does
# this itself: a sandboxed run has only the binaries the cap declares, and a
# shell builtin cannot go missing. The cut counts characters only when the run
# has a UTF-8 locale; otherwise it counts bytes and can end the excerpt inside a
# multi-byte character, which stays cosmetic.
log_text() {
  local s
  s=${1//[$'\001'-$'\037'$'\177']/ }
  s=${s//$'\302'[$'\200'-$'\237']/ }
  printf '%s' "${s:0:${2:-80}}"
}

CLAIM=$(store '{"capability":"telegram","action":"claim_and_read","table":"updates","where":"processed=0","order":"id ASC","set":{"processed":1},"returning":["id","chat_id","sender","text"]}')
CLAIMED=$(echo "$CLAIM" | yesmem json -r '.claimed')
if [ "$CLAIMED" != "true" ]; then
  printf '[%s] reply: no pending\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
fi

# The row is already claimed (processed=1), so an unguarded read here loses the
# message without a trace: no answer, no retry, not even a log line. Same sink as
# the branches below, one line higher (security review of YM-394).
ROW_ID=$(echo "$CLAIM" | yesmem json '.row.id') || ROW_ID=""
TEXT=$(echo "$CLAIM" | yesmem json -r '.row.text') || TEXT=""
SENDER=$(echo "$CLAIM" | yesmem json -r '.row.sender') || SENDER=""
printf '[%s] reply: replying row=%s sender=%s text=%s\n' "$(date -Is)" "$ROW_ID" "$(log_text "$SENDER")" "$(log_text "$TEXT")" >> "$YESMEM_JOB_DIR/treply.log"

CHAT_ID=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["chat_id"],"limit":1}' | yesmem json -r '.rows[0].value') || CHAT_ID=""
TOKEN=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["bot_token"],"limit":1}' | yesmem json -r '.rows[0].value') || TOKEN=""
if [ -z "$TOKEN" ] || [ -z "$CHAT_ID" ]; then printf '[%s] reply: row=%s claimed but no chat_id/token\n' "$(date -Is)" "$ROW_ID" >> "$YESMEM_JOB_DIR/treply.log"; exit 0; fi

# The in-band command branches below (/sessions, /status, /use, /model, /models)
# answer with one short message and then exit 0. The daemon prepends set -e on
# the execute_cap path (handler_cap_exec.go, fireCapHandler), so a bare curl
# whose send failed ended the branch before its own log line: the run looked
# like a crash for a message nobody could have received. A failed send is a
# logged step, not an abort (YM-337). -f makes an API refusal (HTTP 400/403) a
# failed send too, because without it curl exits 0 and the refusal is silent.
reply_send() {
  curl -4 -sS -f -m 10 "https://api.telegram.org/bot${TOKEN}/sendMessage" -d "chat_id=${CHAT_ID}" --data-urlencode "text=$1" > /dev/null || printf '[%s] reply: sendMessage failed\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
}

# --- Command detection (case-insensitive) ---
NAME=""
UPSERT_PAYLOAD=""
EXPLICIT_SESSION=""
shopt -s nocasematch
if [[ "$TEXT" =~ ^/use[[:space:]]+([A-Za-z0-9_-]{1,32})[[:space:]]+([A-Za-z0-9_-]{1,64})$ ]]; then
  NAME="${BASH_REMATCH[1]}"
  EXPLICIT_SESSION="${BASH_REMATCH[2]}"
  UPSERT_PAYLOAD=$(yesmem json -n --arg name "$NAME" --arg sid "$EXPLICIT_SESSION" '{capability:"telegram","action":"upsert","table":"sessions","data":{name:$name,session_id:$sid,is_default:1}}')
elif [[ "$TEXT" =~ ^/use[[:space:]]+([A-Za-z0-9_-]{1,32})$ ]]; then
  NAME="${BASH_REMATCH[1]}"
  EXISTS_PAYLOAD=$(yesmem json -n --arg name "$NAME" '{capability:"telegram","action":"query","table":"sessions","where":"name=?","args":[$name],"limit":1}')
  EXISTS_COUNT=$(store "$EXISTS_PAYLOAD" | yesmem json '.rows | length')
  # Partial-merge upsert: for existing rows, supply only {name, is_default} — session_id and last_used_at are preserved (cap_store upsert only updates fields in data dict).
  if [ "$EXISTS_COUNT" = "1" ]; then
    UPSERT_PAYLOAD=$(yesmem json -n --arg name "$NAME" '{capability:"telegram","action":"upsert","table":"sessions","data":{name:$name,is_default:1}}')
  else
    UPSERT_PAYLOAD=$(yesmem json -n --arg name "$NAME" '{capability:"telegram","action":"upsert","table":"sessions","data":{name:$name,session_id:"",is_default:1}}')
  fi
elif [[ "$TEXT" =~ ^/sessions?$ ]]; then
  ROWS=$(store '{"capability":"telegram","action":"query","table":"sessions","order":"is_default DESC, name ASC"}')
  COUNT=$(echo "$ROWS" | yesmem json '.rows | length')
  if [ -z "$COUNT" ] || [ "$COUNT" = "0" ] || [ "$COUNT" = "null" ]; then
    MSG="Keine Sessions registriert. /use <name> legt eine an."
  else
    MSG=$(echo "$ROWS" | yesmem json -r '"Sessions (\(.rows | length)):\n" + ([.rows[] | "  \(.name)\(if .is_default == 1 then " (*)" else "" end) [\(.session_id // "fresh")]"] | join("\n"))')
  fi
  reply_send "$MSG"
  printf '[%s] reply: /sessions -> %s\n' "$(date -Is)" "${COUNT:-0}" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
elif [[ "$TEXT" =~ ^/status$ ]]; then
  STATUS_ROW=$(store '{"capability":"telegram","action":"query","table":"sessions","where":"is_default=1","limit":1}')
  STATUS_NAME=$(echo "$STATUS_ROW" | yesmem json -r '.rows[0].name // empty')
  STATUS_SID=$(echo "$STATUS_ROW" | yesmem json -r '.rows[0].session_id // empty')
  STATUS_USED=$(echo "$STATUS_ROW" | yesmem json -r '.rows[0].last_used_at // empty')
  if [ -z "$STATUS_NAME" ]; then
    MSG="Keine aktive Session. /use <name> schaltet oder legt an."
  else
    MSG="Aktive Session: ${STATUS_NAME}"
    if [ -n "$STATUS_SID" ]; then
      MSG="${MSG} (session: ${STATUS_SID})"
    else
      MSG="${MSG} (neu, startet beim naechsten Reply)"
    fi
    if [ -n "$STATUS_USED" ]; then
      MSG="${MSG}"$'\n'"Zuletzt genutzt: ${STATUS_USED}"
    fi
  fi
  reply_send "$MSG"
  printf '[%s] reply: /status -> %s\n' "$(date -Is)" "$(log_text "${STATUS_NAME:-none}" 32)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
elif [[ "$TEXT" =~ ^/use([[:space:]]|$) ]]; then
  MSG=$'Usage: /use <name> | /use <name> <session_id>\nName: A-Z a-z 0-9 _ - (max 32).'
  reply_send "$MSG"
  printf '[%s] reply: /use invalid syntax\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
elif [[ "$TEXT" =~ ^/model$ ]]; then
  ACTIVE_ROW=$(store '{"capability":"telegram","action":"query","table":"sessions","where":"is_default=1","limit":1}')
  ACTIVE_NAME=$(echo "$ACTIVE_ROW" | yesmem json -r '.rows[0].name // empty')
  ACTIVE_MODEL=$(echo "$ACTIVE_ROW" | yesmem json -r '.rows[0].model // empty')
  GLOBAL_MODEL=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["reply_model"],"limit":1}' | yesmem json -r '.rows[0].value // "claude-sonnet-4-6"')
  if [ -z "$ACTIVE_NAME" ]; then
    MSG="Keine aktive Session. /use <name> zuerst, dann /model."
  elif [ -n "$ACTIVE_MODEL" ]; then
    MSG="Aktive Session: ${ACTIVE_NAME}\nModel: ${ACTIVE_MODEL} (session-spezifisch)\nGlobaler Default: ${GLOBAL_MODEL}"
  else
    MSG="Aktive Session: ${ACTIVE_NAME}\nModel: ${GLOBAL_MODEL} (globaler Default)\n/session-spezifisch setzen via: /model <name>"
  fi
  reply_send "$MSG"
  printf '[%s] reply: /status model -> %s\n' "$(date -Is)" "$(log_text "${ACTIVE_NAME:-none}" 32)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
elif [[ "$TEXT" =~ ^/model[[:space:]]+([A-Za-z0-9_./-]{1,64})$ ]]; then
  MODEL_ARG="${BASH_REMATCH[1]}"
  ACTIVE_ROW=$(store '{"capability":"telegram","action":"query","table":"sessions","where":"is_default=1","limit":1}')
  ACTIVE_NAME=$(echo "$ACTIVE_ROW" | yesmem json -r '.rows[0].name // empty')
  if [ -z "$ACTIVE_NAME" ]; then
    MSG="Keine aktive Session. /use <name> zuerst."
    reply_send "$MSG"
    printf '[%s] reply: /model failed: no active session\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
    exit 0
  fi
  if [[ "$MODEL_ARG" =~ ^(default|clear|reset)$ ]]; then
    CLEAR_PAYLOAD=$(yesmem json -n --arg name "$ACTIVE_NAME" '{capability:"telegram","action":"upsert","table":"sessions","data":{name:$name,model:null}}')
    echo "$CLEAR_PAYLOAD" | while IFS= read -r p; do store "$p" > /dev/null; done
    GLOBAL_MODEL=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["reply_model"],"limit":1}' | yesmem json -r '.rows[0].value // "claude-sonnet-4-6"')
    MSG="Session ${ACTIVE_NAME}: model cleared → global default (${GLOBAL_MODEL})"
  else
    SET_PAYLOAD=$(yesmem json -n --arg name "$ACTIVE_NAME" --arg model "$MODEL_ARG" '{capability:"telegram","action":"upsert","table":"sessions","data":{name:$name,model:$model}}')
    echo "$SET_PAYLOAD" | while IFS= read -r p; do store "$p" > /dev/null; done
    MSG="Session ${ACTIVE_NAME}: model gesetzt auf ${MODEL_ARG}"
  fi
  reply_send "$MSG"
  printf '[%s] reply: /model -> %s=%s\n' "$(date -Is)" "$(log_text "$ACTIVE_NAME" 32)" "$(log_text "$MODEL_ARG" 64)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
elif [[ "$TEXT" =~ ^/model([[:space:]]|$) ]]; then
  MSG=$'Usage: /model | /model <name> | /model default\nName: A-Z a-z 0-9 _ . / - (max 64).'
  reply_send "$MSG"
  printf '[%s] reply: /model invalid syntax\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
elif [[ "$TEXT" =~ ^/models$ ]]; then
  # A sandboxed run gets the list from the daemon: ~/.config/opencode/opencode.json
  # is masked there (plugin paths and mcpServers in it become code at the next
  # opencode start). The file read stays as the fallback for a manual run.
  if [ -n "${YESMEM_OPENCODE_MODELS:-}" ]; then
    MSG="$YESMEM_OPENCODE_MODELS"
  else
    OC_CONFIG="${HOME}/.config/opencode/opencode.json"
    if [ ! -f "$OC_CONFIG" ]; then
      MSG="opencode.json nicht gefunden: $OC_CONFIG"
    else
      # Under the prepended set -e an unreadable config ended the run here, after
      # the message was already claimed: the answer this "2>&1" is meant to send was
      # swallowed with it (cold review of YM-394).
      MSG=$(yesmem json -r < "$OC_CONFIG" '
        [.provider // {} | to_entries[] | .key as $p |
          (.value.models // {}) | keys[]? | "\($p)/\(.)"
        ] as $explicit |
        [.provider // {} | to_entries[] | select((.value.models // {}) == {}) | "\(.key)/* (default)"] as $default
        | ($explicit + $default) | sort | unique | .[]
      ' 2>&1) || MSG="opencode.json nicht lesbar: $OC_CONFIG"
    fi
  fi
  reply_send "$MSG"
  printf '[%s] reply: /models\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
fi
shopt -u nocasematch

# --- /use switch execution (shared by both /use branches) ---
if [ -n "$NAME" ] && [ -n "$UPSERT_PAYLOAD" ]; then
  # Reorder: set new default FIRST, then reset others. Avoids window with no is_default=1 row.
  echo "$UPSERT_PAYLOAD" | while IFS= read -r p; do store "$p" > /dev/null; done
  # Reset is_default on every OTHER row that currently has is_default=1.
  # Bug fix: previous version wrapped in [] producing a single JSON array line;
  # the `while read` loop processed only that one line, store failed silently,
  # and stale is_default=1 rows survived. Now jq emits one object per line.
  OTHERS=$(store '{"capability":"telegram","action":"query","table":"sessions","where":"is_default=1","order":"id ASC"}')
  echo "$OTHERS" | yesmem json -r --arg NAME "$NAME" '.rows[] | select(.name != $NAME) | {capability:"telegram",action:"upsert",table:"sessions",data:{name:.name,is_default:0}}' | while IFS= read -r p; do store "$p" > /dev/null; done
  MSG="Aktive Session: ${NAME}"
  if [ -n "$EXPLICIT_SESSION" ]; then MSG="${MSG}"$'\n'"Session: ${EXPLICIT_SESSION}"; fi
  reply_send "$MSG"
  printf '[%s] reply: /use -> %s\n' "$(date -Is)" "$(log_text "$NAME" 32)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
fi

# --- Normal reply path ---

# Ensure sessions table exists (auto-create if missing — robustness against pre-setup installs).
# Probe via direct query instead of list_tables (which breaks if cap_store_meta has orphan rows
# referencing dropped tables — see Learning #80227). Query returns a clean "does not exist" error
# string when the table is missing, which we detect explicitly.
SESS_PROBE=$(store '{"capability":"telegram","action":"query","table":"sessions","limit":1}' 2>&1)
if echo "$SESS_PROBE" | grep -q "does not exist"; then
  store '{"capability":"telegram","action":"create_table","table":"sessions","columns":[{"name":"name","type":"TEXT","unique":true},{"name":"session_id","type":"TEXT"},{"name":"is_default","type":"INTEGER"},{"name":"model","type":"TEXT"},{"name":"last_used_at","type":"TEXT"}]}' > /dev/null 2>&1
  printf '[%s] reply: auto-created sessions table\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
fi

# Lazy migration: seed sessions table from legacy reply_session config if empty (one-shot)
SESS_COUNT=$(store '{"capability":"telegram","action":"query","table":"sessions","limit":1}' | yesmem json '.rows | length')
if [ -z "$SESS_COUNT" ] || [ "$SESS_COUNT" = "0" ] || [ "$SESS_COUNT" = "null" ]; then
  LEGACY_SESSION=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["reply_session"],"limit":1}' | yesmem json -r '.rows[0].value // empty')
  if [ -n "$LEGACY_SESSION" ]; then
    SEED=$(yesmem json -n --arg name "default" --arg sid "$LEGACY_SESSION" '{capability:"telegram","action":"upsert","table":"sessions","data":{name:$name,session_id:$sid,is_default:1}}')
    echo "$SEED" | while IFS= read -r p; do store "$p" > /dev/null; done
    # Clear legacy reply_session so re-seed cannot fire after manual row deletion
    CLEAR=$(yesmem json -n '{capability:"telegram","action":"upsert","table":"config","data":{key:"reply_session",value:""}}')
    echo "$CLEAR" | while IFS= read -r p; do store "$p" > /dev/null; done
    printf '[%s] reply: seeded default session from reply_session config (legacy cleared)\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  fi
fi

# Read active session_id (sessions table preferred, fallback to reply_session config)
SESSION_ID=$(store '{"capability":"telegram","action":"query","table":"sessions","where":"is_default=1","limit":1}' | yesmem json -r '.rows[0].session_id // empty')
if [ -z "$SESSION_ID" ]; then
  SESSION_ID=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["reply_session"],"limit":1}' | yesmem json -r '.rows[0].value // empty')
fi

MODEL=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["reply_model"],"limit":1}' | yesmem json -r '.rows[0].value // "claude-sonnet-4-6"')
# Per-session model overrides global reply_model when set
SESSION_MODEL=$(store '{"capability":"telegram","action":"query","table":"sessions","where":"is_default=1","limit":1}' | yesmem json -r '.rows[0].model // empty')
if [ -n "$SESSION_MODEL" ]; then
  MODEL="$SESSION_MODEL"
fi
SYSPROMPT=$(store '{"capability":"telegram","action":"query","table":"config","where":"key=?","args":["system_prompt"],"limit":1}' | yesmem json -r '.rows[0].value // "Du bist ein hilfreicher Assistent."')

# The assignment takes the status of the substitution: under the prepended set -e
# a failing llm ended the run here, before the check on the next line could read
# $?, so the branch below was dead code and the job counted as failed with no log
# line (YM-394, the same sink YM-368 closed in the poll).
LLM_EXIT=0
RESULT=$(llm "$MODEL" "$SYSPROMPT" "Nachricht von $SENDER: $TEXT" "$SESSION_ID" "tools") || LLM_EXIT=$?
if [ "$LLM_EXIT" -ne 0 ]; then
  printf '[%s] reply: llm failed exit=%s\n' "$(date -Is)" "$LLM_EXIT" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
fi

if ! echo "$RESULT" | yesmem json -e '.' > /dev/null 2>&1; then
  printf '[%s] reply: invalid llm JSON\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
fi

REPLY=$(echo "$RESULT" | yesmem json -r '.result // empty')
REPLY=$(echo "$REPLY" | sed '/^\[[0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\} [0-9]\{2\}:[0-9]\{2\}:[0-9]\{2\}\] \[msg:[0-9]\+\]/d')
if [ -z "$REPLY" ]; then
  printf '[%s] reply: empty reply from llm\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
fi

NEW_SESSION=$(echo "$RESULT" | yesmem json -r '.session_id // empty')
if [ -n "$NEW_SESSION" ]; then
  ACTIVE_NAME=$(store '{"capability":"telegram","action":"query","table":"sessions","where":"is_default=1","limit":1}' | yesmem json -r '.rows[0].name // empty')
  if [ -n "$ACTIVE_NAME" ]; then
    NOW=$(date -Is)
    WB=$(yesmem json -n --arg name "$ACTIVE_NAME" --arg sid "$NEW_SESSION" --arg ts "$NOW" '{capability:"telegram","action":"upsert","table":"sessions","data":{name:$name,session_id:$sid,is_default:1,last_used_at:$ts}}')
    echo "$WB" | while IFS= read -r p; do store "$p" > /dev/null; done
  else
    SP=$(yesmem json -n --arg key "reply_session" --arg value "$NEW_SESSION" '{capability:"telegram","action":"upsert","table":"config","data":{key:$key,value:$value}}')
    echo "$SP" | while IFS= read -r p; do store "$p" > /dev/null; done
  fi
fi

# The assignment takes the status of the substitution: under set -e a transport
# failure used to end the run here, before the check below could read it
# (YM-337). The "||" keeps the failure in CURL_EXIT instead.
CURL_EXIT=0
SEND=$(curl -4 -s -m 10 "https://api.telegram.org/bot${TOKEN}/sendMessage" -d "chat_id=${CHAT_ID}" --data-urlencode "text=${REPLY}") || CURL_EXIT=$?
if [ "$CURL_EXIT" -ne 0 ] || [ "$(echo "$SEND" | yesmem json -r '.ok // false')" != "true" ]; then
  printf '[%s] reply: sendMessage failed\n' "$(date -Is)" >> "$YESMEM_JOB_DIR/treply.log"
  exit 0
fi
printf '[%s] reply: sent row=%s > %s\n' "$(date -Is)" "$ROW_ID" "$(log_text "$REPLY" 60)" >> "$YESMEM_JOB_DIR/treply.log"
```

## Database

```sql
CREATE TABLE IF NOT EXISTS cap_telegram__config (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    key TEXT NOT NULL UNIQUE,
    value TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cap_telegram__conversations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sender TEXT NOT NULL,
    role TEXT NOT NULL,
    text TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cap_telegram__updates (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    telegram_id INTEGER UNIQUE,
    chat_id INTEGER,
    sender TEXT,
    text TEXT,
    direction TEXT,
    processed INTEGER NOT NULL DEFAULT 0,
    date INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cap_telegram__reply_errors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    row_id INTEGER,
    sender TEXT,
    message_text TEXT,
    stage TEXT,
    exit_code INTEGER,
    stderr TEXT,
    stdout TEXT,
    model TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cap_telegram__sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    session_id TEXT,
    is_default INTEGER NOT NULL DEFAULT 0,
    model TEXT,
    last_used_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```
