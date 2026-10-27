---
name: proxy_health
description: "Check proxy and daemon health: API errors (upstream, overload, rate limit), fork call stats (count, tokens, learnings), keepalive ping analysis (cache_write threshold), general errors (daemon timeouts, restarts). Pure shell parsing, no LLM needed. Persists reports to cap_store."
version: 548
tags: [health, monitoring, proxy, daemon, scheduled, keepalive, fork]
requires: [store]
scope: user
tested: true
auto_active: true
---

## Purpose

Check proxy and daemon health: API errors (upstream, overload, rate limit), fork call stats (count, tokens, learnings), keepalive ping analysis (cache_write threshold), general errors (daemon timeouts, restarts). Pure shell parsing, no LLM needed. Persists reports to cap_store.

## Scripts

### proxy_health
kind: tool
sandbox: none

```javascript
async ({hours, alert, project}) => {
  // hours is interpolated into the journalctl --since argument below, and sh()
  // is `sh -c` on a cap that runs without a sandbox profile. Only a number may
  // reach it: the string `1" ; touch x ; echo "` used to run as a command
  // (OPS-68). A window outside 1..168 hours falls back to one hour, the same
  // way a non-number does, because each hour widens seven parallel journal scans.
  const n = Math.trunc(Number(hours));
  hours = Number.isFinite(n) && n > 0 && n <= 168 ? n : 1;
  alert = alert !== false;
  // No default project: the daemon refuses a bare basename once two checkouts
  // share it, and a guessed name would route the alert somewhere nobody reads
  // (YM-281). Callers pass an absolute path.
  project = project || '';
  const since = `${hours} hour${hours>1?'s':''} ago`;
  const plog = `journalctl --user -u yesmem-proxy --since "${since}" --no-pager 2>&1`;

  // Only text the proxy wrote for this request counts: the message after the
  // closing bracket of its own "[req ...]" prefix, or after the journal header
  // when the record has none. Everything further right is quoted text (a commit
  // subject, a project token, a body from the provider) and is not a statement
  // the counters may read. The bash handler reads the same frame (YM-447).
  // A line that has neither a prefix nor a header is the continuation of a
  // foreign value that carried a newline; the proxy did not write it, so it gets
  // an empty frame and counts nowhere.
  const frameBody = (line) => {
    const frame = /\[req [0-9]+/.exec(line);
    let body;
    if (frame) {
      const seg = line.slice(frame.index + frame[0].length);
      const close = seg.indexOf(']');
      body = close >= 0 ? seg.slice(close + 1) : line;
    } else if (/^\d{4}\/\d\d\/\d\d /.test(line)) {
      // a line read from a plain log file starts with the own date already
      body = line;
    } else {
      // "Mon DD HH:MM:SS host yesmem-proxy[pid]: " ends at the first ": "
      const header = /^\S+ +\d+ \d\d:\d\d:\d\d \S+ yesmem[^ :]*(?:\[\d+\])?: /.exec(line);
      if (!header) return '';
      body = line.slice(header[0].length);
    }
    // Colour is decoration (colorRed in front of "upstream error:") and the own
    // timestamp follows the journal header; both sit in front of the message.
    return body.replace(/\x1b\[[0-9;]*m/g, '')
               .replace(/^ ?\d{4}\/\d\d\/\d\d \d\d:\d\d:\d\d /, '')
               .replace(/^ +/, '');
  };
  // The status header is the start of that message up to and including
  // "upstream <status>: "; the body of the provider follows it and is foreign
  // text, so a body quoting counter vocabulary cannot classify the line.
  const frameHeader = (b) => {
    const m = /^upstream [0-9][0-9][0-9]: /.exec(b);
    return m ? b.slice(0, m[0].length) : b;
  };
  const capAPIError = /^(upstream error:|upstream [45][0-9][0-9]:|\[keepalive\] ping error tid=|fork .*: API error:)/;
  // A commit line is a note about a tool_result and its subject is client text:
  // it quotes any vocabulary a counter reads, so it is out of every selection.
  // The frame filter below runs before the cap, not after it: a forged line that
  // survives the grep must not take one of the fifty slots away from a real one.
  const noCommitNote = ' | grep -v "git commit detected"';

  const apiCap = 50;
  const [apiRaw, forkRaw, forkResultRaw, kaRaw, kaTimeoutRaw, restarts, errRaw] = await Promise.all([
    // Only a line the proxy writes as an error counts: the transport failure, a
    // non-200 upstream answer, a failed keepalive ping or a fork provider error.
    // A bare status number anywhere in the log made a tool_call_id ending in
    // 3429 look like a rate limit (YM-289).
    sh(`${plog}${noCommitNote} | grep -E "upstream error:|upstream [45][0-9][0-9]:|ping error tid=|fork .*: API error:" | head -200`, 15000),
    sh(`${plog}${noCommitNote} | grep "fork extract_and_evaluate" | head -50`, 15000),
    sh(`${plog}${noCommitNote} | grep "\\[fork\\] result:" | head -50`, 15000),
    // Successful pings only: the error line counts as an API error, and mixing
    // the two reported attempts as if they had warmed the cache.
    sh(`${plog}${noCommitNote} | grep "\\[keepalive\\] ping tid=" | head -50`, 15000),
    // A ping that ran into its own budget is a large-context signal, not an
    // upstream answer: the producer names the size ("ping timeout tid=...
    // ctx_tokens=...", YM-347). Read on its own line, because a 750k-token
    // session timing out used to read as a provider outage and pushed the
    // report to degraded with no upstream error anywhere.
    sh(`${plog}${noCommitNote} | grep "\\[keepalive\\] ping timeout tid=" | head -50`, 15000),
    // The startup line carries its thresholds; text that merely mentions a
    // listener (a fork preview quoting model output) is not a restart.
    sh(`${plog}${noCommitNote} | grep -c "listening on .*threshold=" || echo 0`, 10000),
    // A framed provider failure is counted in A; the daemon-timeout number must
    // not count it a second time (the bash handler excludes the same shape).
    sh(`${plog}${noCommitNote} | grep -iE "ERR|panic|fatal|refused|timeout" | grep -viE "REWRITE|fork|associative|keepalive|upstream error:" | head -30`, 15000)
  ]);

  const apiLines = apiRaw.trim().split('\n').filter(l => l.trim() && capAPIError.test(frameBody(l))).slice(0, apiCap);
  const apiErrors = {total: apiLines.length, by_type: {}};
  for (const l of apiLines) {
    const h = frameHeader(frameBody(l));
    const type = h.includes('context canceled') ? 'context_canceled' :
                 h.includes('closed network') ? 'connection_closed' :
                 /overload|upstream 529:|HTTP 529 body=/.test(h) ? 'overloaded' :
                 /upstream 429:|HTTP 429 body=|rate_limit|rate limit/.test(h) ? 'rate_limited' :
                 h.includes('upstream 502') ? 'bad_gateway' :
                 h.includes('upstream 503') ? 'service_unavailable' :
                 h.includes('upstream 500') ? 'internal_server_error' : 'other';
    apiErrors.by_type[type] = (apiErrors.by_type[type] || 0) + 1;
  }

  const forkLines = forkRaw.trim().split('\n').filter(l => l.trim());
  const forkResults = forkResultRaw.trim().split('\n').filter(l => l.trim());
  let totalIn=0, totalCached=0, totalOut=0, totalLearnings=0, totalEvals=0;
  for (const l of forkLines) {
    const m = l.match(/(\d+)\s+in\s*\/\s*(\d+)\s+cached\s*\/\s*(\d+)\s+out/);
    if (m) { totalIn += +m[1]; totalCached += +m[2]; totalOut += +m[3]; }
  }
  for (const l of forkResults) {
    const m = l.match(/(\d+)\s+learnings?/); if (m) totalLearnings += +m[1];
    const e = l.match(/(\d+)\s+evaluations?/); if (e) totalEvals += +e[1];
  }
  const forks = {count: forkLines.length, tokens: {in: totalIn, cached: totalCached, out: totalOut}, learnings: totalLearnings, evaluations: totalEvals};

  const kaLines = kaRaw.trim().split('\n').filter(l => l.trim());
  let kaHealthy=0, kaOver1k=0;
  const kaIssues = [];
  for (const l of kaLines) {
    const cw = l.match(/cache_write=(\d+)/);
    if (cw) { const w = +cw[1]; if (w < 1000) kaHealthy++; else { kaOver1k++; kaIssues.push(`cache_write=${w} in: ${l.slice(l.indexOf('[keepalive]'), l.indexOf('[keepalive]')+80)}`); } }
  }
  const kaTimeoutLines = kaTimeoutRaw.trim().split('\n').filter(l => l.trim());
  let kaMaxCtx = 0;
  for (const l of kaTimeoutLines) {
    const m = l.match(/ctx_tokens=(\d+)/);
    // A forged or truncated journal line must not put an unreadable number into
    // the report; the size is diagnostic, so it is clamped instead of trusted.
    if (m) { const v = Math.min(+m[1], 1e9); if (v > kaMaxCtx) kaMaxCtx = v; }
  }
  const keepalive = {total: kaLines.length, healthy: kaHealthy, cache_write_over_1k: kaOver1k, timeouts: kaTimeoutLines.length, largest_timeout_context: kaMaxCtx, issues: kaIssues.slice(0,5)};

  const errLines = errRaw.trim().split('\n').filter(l => l.trim());
  const daemonTimeouts = errLines.filter(l => l.includes('i/o timeout')).length;
  const general = {daemon_timeouts: daemonTimeouts, other_errors: errLines.length - daemonTimeouts, proxy_restarts: parseInt(restarts.trim()) || 0};

  // Degraded rules first, critical rules after: a failing keepalive also means
  // zero successful pings, and it must not mask a critical condition (the bash
  // handler orders them the same way). Only proxy-side signals are read here,
  // so scheduler state (SF) is not part of this variant.
  let status = 'healthy';
  if (apiErrors.total > 5 || daemonTimeouts > 3 || kaOver1k > 0 || general.proxy_restarts > 2) status = 'degraded';
  // A quiet window is not a degraded one: the bash handler carries the same
  // guard (TOTAL_OTHER > 0). Without it an idle proxy read as degraded, and the
  // timeout rule below could not speak for itself in exactly the live case
  // (large context, no warmed ping left).
  const otherSignals = apiErrors.total + forks.count + daemonTimeouts + general.proxy_restarts;
  if (keepalive.total === 0 && otherSignals > 0) status = 'degraded';
  // A timeout is a large-context signal of its own: the cache of that session
  // is not kept warm, and the wording below names it so nobody looks for an
  // upstream failure that never happened. Three of them have to collect first,
  // the tolerance the API rule uses: one timeout in a 1..168h window is a
  // session that ended, and a standing degraded verdict is exactly the report
  // this ticket was filed about.
  if (keepalive.timeouts > 2) status = 'degraded';
  if (apiErrors.by_type.overloaded > 0 || apiErrors.by_type.rate_limited > 3 || general.proxy_restarts > 5) status = 'critical';

  const ts = new Date().toISOString().slice(0,19);
  const result = {timestamp: ts, status, hours, api_errors: apiErrors, forks, keepalive, general};

  await mcp__yesmem__cap_store({capability:'proxy_health',action:'create_table',table:'reports',columns:JSON.stringify([{name:'timestamp',type:'TEXT'},{name:'status',type:'TEXT'},{name:'summary',type:'TEXT'},{name:'issues_json',type:'TEXT'},{name:'hours',type:'INTEGER'}])});
  const summary = `${status}: ${apiErrors.total} API err, ${forks.count} forks (${totalLearnings} learnings), ${keepalive.total} pings (${kaOver1k} over 1k cw)${keepalive.timeouts > 0 ? `, ${keepalive.timeouts} ping timeouts (up to ${kaMaxCtx} tokens)` : ''}, ${daemonTimeouts} daemon timeouts`;
  await mcp__yesmem__cap_store({capability:'proxy_health',action:'upsert',table:'reports',data:JSON.stringify({timestamp:ts, status, summary, issues_json: JSON.stringify(result), hours})});

  if (alert && status !== 'healthy') {
    const lines = [`PROXY HEALTH [${status.toUpperCase()}] (${since}):`, summary];
    if (kaOver1k > 0) lines.push(`${kaOver1k} keepalive pings with cache_write >= 1k`);
    if (keepalive.timeouts > 0) lines.push(`${keepalive.timeouts} keepalive pings timed out (largest context ${kaMaxCtx} tokens): the prompt outgrew the ping budget, not an upstream error`);
    if (apiErrors.total > 0) lines.push(`API errors: ${JSON.stringify(apiErrors.by_type)}`);
    await mcp__yesmem__broadcast({project, content: lines.join('\n')});
  }

  return result;
}
```

### proxy_health_bash
kind: handler
timeout: 120
sandbox: none

```bash
HOURS=${HOURS:-1}
# HOURS is interpolated into the report JSON as a bare number and into two
# journalctl --since windows. This handler reads each unit once, so the cost per
# hour is the window size times two scans; the JS variant pays for its own set.
# Anything but a decimal number in 1..168 falls back to one hour: hours=0 asks
# for a window that ends now, and an arbitrary size reached the report and the
# scans unclamped (YM-299). A fraction counts as a non-number here; the JS
# variant truncates it instead (8.5 becomes 8).
# The digits are listed one by one because a range such as 0-9 follows the
# process collation: under de_DE.UTF-8 it also matches non-ASCII digits (٣),
# which then pass the guard and produce invalid JSON.
case "$HOURS" in ''|*[!0123456789]*) HOURS=1 ;; esac
# Leading zeros are digits, but "hours":0008 is not valid JSON. The strip stays
# bounded: each removal copies the remaining string and a long run of zeros makes
# the pattern work backtrack (20000 zeros took 52s, a single-expansion variant
# 12s for 40000 zeros, against this cap's 120s timeout), so no padded hour count
# is longer than eight digits.
while [ "${#HOURS}" -le 8 ] && [ "${HOURS#0}" != "$HOURS" ]; do HOURS=${HOURS#0}; done
# All zeros strip to nothing; what remains starts at a digit above zero and is at
# most eight characters long, so only the upper bound needs a comparison.
if [ -z "$HOURS" ] || [ "${#HOURS}" -gt 3 ] || [ "$HOURS" -gt 168 ]; then
  HOURS=1
fi
ALERT=${ALERT:-true}
# The daemon refuses a bare basename once two checkouts share it (live: "yesmem"
# matched /home/tester/memory/yesmem and /home/tester/projects/yesmem, and the
# broadcast came back as an error). Scheduled runs get their own project as an
# absolute path in YESMEM_PROJECT; no fallback, because any other source is a
# guess about who should receive the alert.
PROJECT=${YESMEM_PROJECT:-}
# PROJECT is interpolated into the broadcast JSON; quotes, backslashes, line
# breaks and control characters would let it add fields or corrupt the payload
# (nc drops the error).
PROJECT=${PROJECT//\"/}
PROJECT=${PROJECT//\\/}
PROJECT=$(printf '%s' "$PROJECT" | tr -d '\000-\037\177')
SINCE="$HOURS hour ago"
[ "$HOURS" != "1" ] && SINCE="$HOURS hours ago"
# The broadcast goes to the daemon socket. A sandboxed run only has the
# restricted job socket mounted, and it is told so in YESMEM_SOCK — a hardcoded
# daemon.sock is masked inside the sandbox and the report would go nowhere.
SOCK="${YESMEM_SOCK:-$HOME/.claude/yesmem/daemon.sock}"

# One journalctl pass per unit. The counters used to run one journalctl per
# metric, fourteen passes over the same window at roughly thirty seconds each,
# so the job could never meet a sane timeout.
PROXY_COUNTS=$(journalctl --user -u yesmem-proxy --since "$SINCE" --no-pager 2>/dev/null | awk '
  # The proxy colours its own lines ("%supstream error: ..." carries colorRed in
  # front of the message), so the escape sequence is decoration that sits where
  # the frame starts. Built from the code point: the octal escape is not portable
  # across awk implementations.
  BEGIN { esc = sprintf("%c", 27) }
  # Fork diagnostics quote model output back into this journal. Whatever words
  # that output contains, the line is not a proxy error, so it ends here before
  # any counter below can read it.
  # An upstream body is foreign text: it may quote the words the counters look
  # for ("cache:", "[keepalive] ping", "listening on ... threshold="), which
  # moved the structural counters of that line and diluted a real cache total.
  # Those counters read the status header only, the record up to and including
  # "upstream <status>: ". The failure-typing counters below keep reading the
  # rest of the line: the reason a provider gives ("invalid api key", a 429
  # body) is the evidence there, and no header carries it.
  # The thread id sits between "tid=" and "]", comes from the client, and may
  # quote counter vocabulary itself. It is not evidence for any counter, so it
  # is removed from the record first; the line as written keeps it.
  {
    rest = $0
    # Only the request prefix carries the id in brackets: " tid=<id>]". A
    # keepalive line names its tid without a bracket and keeps its line.
    gsub(/ tid=[^]]*]/, " tid=]", rest)
    # The frame the proxy wrote for this record: the message after the closing
    # bracket of its own request prefix, or after the journal header when the
    # record has none. Everything further right is text the proxy only quotes (a
    # commit subject, a file path, a provider body) and is not a statement a
    # counter may read (YM-447). The project name sits inside the prefix, so a
    # record whose slot carries vocabulary still counts nowhere.
    body = rest
    if (match(rest, /\[req [0-9]+/)) {
      seg = substr(rest, RSTART + RLENGTH)
      if (match(seg, /^[^]]*\]/)) body = substr(seg, RSTART + RLENGTH)
    } else if (rest ~ /^[0-9][0-9][0-9][0-9]\//) {
      # a line read from a plain log file starts with the own date already
    } else if (match(rest, /^[^ ]+ +[0-9]+ [0-9][0-9]:[0-9][0-9]:[0-9][0-9] [^ ]+ yesmem[^ :]*(\[[0-9]+\])?: /)) {
      # "Mon DD HH:MM:SS host yesmem-proxy[pid]: " — the header of the unit
      body = substr(rest, RSTART + RLENGTH)
    } else {
      # Neither prefix nor header: the continuation of a foreign value that
      # carried a newline. The proxy did not write this record, so it counts
      # nowhere (YM-447).
      next
    }
    # Colour is decoration (colorRed in front of "upstream error:") and the own
    # timestamp follows the journal header; both sit in front of the message.
    # Built from the code point: the octal escape is not portable across awk
    # implementations.
    gsub(esc "\\[[0-9;]*m", "", body)
    sub(/^[0-9][0-9][0-9][0-9]\/[0-9][0-9]\/[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9] /, "", body)
    # The status header is the start of that message up to and including
    # "upstream <status>: ". The body of the provider follows it and is foreign
    # text.
    hdr = body
    if (match(hdr, /^ *upstream [0-9][0-9][0-9]: /)) hdr = substr(hdr, 1, RSTART + RLENGTH - 1)
    # The commit detection line is a note about a tool_result, not a signal: its
    # subject is client text and can quote any vocabulary, including a restart
    # line or an i/o timeout. It counts nowhere (YM-447).
    if (body ~ /^ *git commit detected: /) next
  }
  rest ~ /\[fork\] extract_and_evaluate: parse error/ { fork++; next }
  rest ~ /\[fork\][ ]+learning / { next }
  # An error is only what the proxy writes as one: the transport failure line
  # ("upstream error: ..."), a non-200 answer ("upstream 429: ..."), a failed
  # keepalive ping ("ping error tid=... HTTP 429 body=...") or a fork provider
  # failure ("fork x: API error: ..."). Counting a bare 429 anywhere in the log
  # read a tool_call_id ending in 3429 as a rate limit and alerted CRITICAL on
  # an idle proxy (YM-289), so every branch below hangs off that framing.
  # The frame has a position as well: it is the start of the message the proxy
  # wrote for this record (body, above). Text further right is quoted and does
  # not count, which is what keeps a commit subject or a project token naming an
  # error out of the report (YM-447). Once a
  # line is framed, its numbers and words are safe to read: the status digit and
  # the error body are what identify the failure.
  # A ping that ran into its own budget is a large-context signal, not an
  # upstream answer: the proxy names the size in that line ("ping timeout
  # tid=... ctx_tokens=...", YM-347). It gets its own counter, so a 750k-token
  # session does not read like a provider outage.
  hdr ~ /\[keepalive\] ping timeout tid=/ {
    kato++
    # The size is what makes the report actionable, and awk keeps the largest of
    # the window. Clamped, because a forged or truncated line must not put an
    # unreadable number into the summary.
    if (match(hdr, /ctx_tokens=[0-9]+/)) {
      v = substr(hdr, RSTART + 11, RLENGTH - 11) + 0
      if (v > 1000000000) v = 1000000000
      if (v > kamax) kamax = v
    }
    next
  }
  body ~ /^ *(upstream error:|upstream [45][0-9][0-9]:|\[keepalive\] ping error tid=|fork .*: API error:)/ {
    api++
    # The status is matched where the proxy writes it, never as a bare number:
    # a thread id like ses_x529y would otherwise read as an overload, and a
    # project token that quotes the vocabulary would classify a real error.
    if (hdr ~ /overload|upstream 529:|HTTP 529 body=/) ovld++
    if (hdr ~ /upstream 429:|HTTP 429 body=|rate_limit|rate limit/) rate++
    if (body ~ /invalid.*api.*key|provider.*fail|deepseek.*error/) ds++
    if (hdr ~ /connection refused|no such host|dial tcp/) conn++
  }
  hdr ~ /fork extract_and_evaluate/ { fork++ }
  # A successful ping is the one with cache accounting; the error line above is
  # an attempt, not a ping the keepalive can be judged by.
  hdr ~ /\[keepalive\] ping tid=/ {
    ka++
    if (match(hdr, /cache_write=[0-9]+/)) {
      v = substr(hdr, RSTART + 12, RLENGTH - 12) + 0
      if (v >= 1000) kaov++
    }
  }
  hdr ~ /listening on .*threshold=/ { restarts++ }
  # A timeout only counts when it comes from an error line the proxy writes, and
  # only on the daemon side: an upstream read timeout is a provider failure and
  # belongs in A/N. The proxy labels its daemon failures differently per call
  # site ("daemon error:", "coding-query search error:", "read body error:",
  # "daemon err="), so the rule reads the error marker, not one label.
  hdr ~ /(error:|err=).*i\/o timeout/ && hdr !~ /upstream error:/ { dto++ }
  hdr ~ /cache:/ {
    cache++
    if (match(hdr, /\([0-9]+\.?[0-9]*% hit\)/)) {
      pct = substr(hdr, RSTART + 1, RLENGTH - 1) + 0
      if (pct < 50) cachelow++
    }
  }
  END { printf "%d %d %d %d %d %d %d %d %d %d %d %d %d %d", api, ovld, rate, fork, ka, kaov, kato, kamax, restarts, dto, cache, cachelow, ds, conn }
')
read -r API_TOTAL API_OVLD API_RATE FORK_COUNT KA_TOTAL KA_OV1K KA_TIMEOUT KA_MAX_TOKENS RESTARTS DAEMON_TO CACHE_TOTAL CACHE_LOW DS_ERR CONN_ERR <<< "$PROXY_COUNTS"

DAEMON_COUNTS=$(journalctl --user -u yesmem --since "$SINCE" --no-pager 2>/dev/null | awk '
  # A job that prints nothing is not a finding: two scheduled jobs answer every
  # 15 seconds with "OK (0 bytes)" by design, and counting them put a four-digit
  # number into the alert line (YM-289). A failure is only the FAILED line.
  /scheduler.*bash job.*FAILED/ { failed++ }
  # A headless job fails with its own wording: executeHeadless writes "headless
  # job <id> failed: <err>", "headless job <id> timed out after <d>", and, when
  # the run never starts (a sandbox profile whose ai-jail is missing), "headless
  # job <id>: <err>". Reading only the bash form made a scheduler whose only
  # failing work is headless look healthy (YM-293). The id is the only token
  # before that colon, which keeps the "skipped: <reason>" line out: a skipped
  # run never started by design and is not a failure.
  /scheduler.*headless job [^ :]+ failed: / { failed++; next }
  /scheduler.*headless job [^ :]+ timed out after / { failed++; next }
  /scheduler.*headless job [^ :]+: / { failed++; next }
  END { printf "%d", failed }
')
read -r SCHED_FAIL <<< "$DAEMON_COUNTS"

# A counter is empty when awk wrote no row for it, and every empty one falls back
# to zero here. The previous form, eval "[ -z ... ] && $v=0", returned 1 whenever
# the variable WAS set, and the daemon runs a bash handler under a prepended
# set -e (handler_cap_exec.go, fireCapHandler): the first non-empty counter
# ended the run before store() ever ran (YM-327). The "||" form has a zero exit
# status on both paths, and printf -v keeps the value out of the shell parser
# instead of handing it to eval.
#
# A counter is only a number once this loop has proven it. The arithmetic
# expansions below (TOTAL_OTHER, CACHE_TOTAL/2) read their operands as an
# expression, and a bash arithmetic array subscript runs a command substitution:
# the value "a[$(touch>f)]" executed touch (measured 19.10.2026, YM-337). Every
# source today is awk 'printf %d', so this is the seam for the next reader, not
# a present failure. Leading zeros go too, because $((08)) is an octal error and
# not eight. A value that is not a number is not zero: it means the read broke,
# and a broken read must not come out as "healthy" (GARBLED below). An empty
# value is the case this loop was built for, an awk that wrote no row for it,
# and it keeps the old verdict of zero: whether that is a quiet window or a
# journalctl that never answered cannot be decided from the value (YM-327).
GARBLED=0
for v in API_TOTAL API_OVLD API_RATE FORK_COUNT KA_TOTAL KA_OV1K KA_TIMEOUT KA_MAX_TOKENS RESTARTS DAEMON_TO CACHE_TOTAL CACHE_LOW DS_ERR CONN_ERR SCHED_FAIL; do
  val=${!v:-}
  case "$val" in
    '') val=0 ;;
    *[!0123456789]*) val=0; GARBLED=1 ;;
  esac
  # A value up to 24 characters is stripped of its padding; a longer field falls
  # to the length check below and is flagged there. The bound keeps the loop
  # linear, and no awk 'printf %d' pads at all.
  while [ "${#val}" -le 24 ] && [ "${val#0}" != "$val" ]; do val=${val#0}; done
  if [ -z "$val" ]; then
    val=0
  elif [ "${#val}" -gt 18 ]; then
    # Eighteen digits is more than any counter. A longer field never came out of
    # awk 'printf %d', so the read is broken, not the counter.
    val=0; GARBLED=1
  fi
  printf -v "$v" '%s' "$val"
done

TOTAL_OTHER=$((API_TOTAL + FORK_COUNT + RESTARTS + DAEMON_TO + CACHE_TOTAL + DS_ERR + CONN_ERR))
STATUS="healthy"
[ "$API_TOTAL" -gt 5 ] 2>/dev/null && STATUS="degraded"
[ "$DAEMON_TO" -gt 3 ] 2>/dev/null && STATUS="degraded"
[ "$KA_OV1K" -gt 0 ] 2>/dev/null && STATUS="degraded"
# Three timeouts have to collect first, the tolerance the API rule uses: one
# timeout in a 1..168h window is a session that ended, and a standing degraded
# verdict is exactly the report this ticket was filed about.
[ "$KA_TIMEOUT" -gt 2 ] 2>/dev/null && STATUS="degraded"
[ "$RESTARTS" -gt 2 ] 2>/dev/null && STATUS="degraded"
[ "$SCHED_FAIL" -gt 0 ] 2>/dev/null && STATUS="degraded"
# A counter the gate had to reject means the read broke. The ladder below can
# only speak for numbers it actually got, so the run says so instead of healthy.
[ "$GARBLED" -gt 0 ] 2>/dev/null && STATUS="degraded"
[ "$CACHE_TOTAL" -gt 10 ] 2>/dev/null && [ "$CACHE_LOW" -gt $((CACHE_TOTAL / 2)) ] 2>/dev/null && STATUS="degraded"
[ "$KA_TOTAL" -eq 0 ] 2>/dev/null && [ "$HOURS" -ge 1 ] 2>/dev/null && [ "$TOTAL_OTHER" -gt 0 ] 2>/dev/null && STATUS="degraded"
[ "$API_OVLD" -gt 0 ] 2>/dev/null && STATUS="critical"
[ "$API_RATE" -gt 3 ] 2>/dev/null && STATUS="critical"
[ "$RESTARTS" -gt 5 ] 2>/dev/null && STATUS="critical"
[ "$DS_ERR" -gt 0 ] 2>/dev/null && STATUS="critical"
[ "$CONN_ERR" -gt 10 ] 2>/dev/null && STATUS="critical"

TS=$(date -Is | cut -d'+' -f1)
# The timeout count rides inside the K token and only when it is non-zero: the
# established "K<pings>(<over 1k>)" form stays readable for every window without
# a timeout (YM-347).
KA_TIMEOUT_SUFFIX=""
if [ "$KA_TIMEOUT" -gt 0 ] 2>/dev/null; then KA_TIMEOUT_SUFFIX="/$KA_TIMEOUT"; fi
# The K token carries only the count, so the summary names the cause where one
# exists: without this sentence "K0(0/3)" reads like a provider outage, which is
# the misdiagnosis the ticket is about. It sits in the summary, not only in the
# broadcast, so the stored report keeps it too.
KA_TIMEOUT_NOTE=""
if [ "$KA_TIMEOUT" -gt 0 ] 2>/dev/null; then
  KA_TIMEOUT_NOTE=" | $KA_TIMEOUT ping timeout(s): prompt exceeded the ping budget (largest context $KA_MAX_TOKENS tokens), not an upstream error"
fi
# A degraded verdict whose counters all read zero needs its cause in the summary,
# or it repeats the misdiagnosis this report exists to avoid. Same reason the
# ping-timeout sentence sits here.
GARBLED_NOTE=""
if [ "$GARBLED" -gt 0 ] 2>/dev/null; then
  GARBLED_NOTE=" | counter read broken: a counter value was not a number or longer than 18 digits and counts as zero"
fi
SUMMARY="$STATUS: A$API_TOTAL($API_OVLD/$API_RATE) F$FORK_COUNT K$KA_TOTAL($KA_OV1K$KA_TIMEOUT_SUFFIX) R$RESTARTS SF$SCHED_FAIL C$CACHE_LOW/$CACHE_TOTAL D$DS_ERR N$CONN_ERR T$DAEMON_TO$KA_TIMEOUT_NOTE$GARBLED_NOTE"

# The JSON payloads are built with printf, never with nested double quotes: an
# unescaped quote inside the assignment broke the whole script (exit 2).
REPORT=$(printf '{"capability":"proxy_health","action":"upsert","table":"reports","data":{"timestamp":"%s","status":"%s","summary":"%s","hours":%s}}' "$TS" "$STATUS" "$SUMMARY" "$HOURS")
# A failed store loses the report, but it must not cost the alert: the health
# signal is most needed when the bookkeeping is broken. The error is reported at
# once, so a later exit (a rejected broadcast) cannot hide it, and the run still
# ends non-zero.
# Text this script does not own (the socket path, the daemon's answer, the store
# error) reaches stderr, and the scheduler keeps job output as text: a newline
# or a C0 control character in it would split one message into a forged second
# line. Each quoted value is filtered to at most 200 bytes, so a message that
# quotes two of them stays one line. The filter is byte based and leaves a
# Unicode line separator (U+0085, U+2028) alone, because no reader on this path
# treats those as a line break and deleting their bytes would corrupt every
# character that shares them.
safe_log() { printf '%s' "$1" | tr -d '\000-\037\177' | cut -c1-200; }

# A failed store has two causes that need different words: the write was
# refused (bookkeeping), or the socket the yesmem CLI talks to is gone after a
# daemon restart, which is an outage of its own. Both used to read only as
# "report not stored", which hid the second one (YM-337).
STORE_FAILED=0
if ! STORE_ERR=$(store "$REPORT" 2>&1 >/dev/null); then
  STORE_FAILED=1
  case "$STORE_ERR" in
    *"dial unix"*|*"connect: no such file"*|*"connection refused"*)
      printf 'proxy_health: job socket unavailable at %s: %s\n' "$(safe_log "$SOCK")" "$(safe_log "$STORE_ERR")" >&2
      ;;
    *)
      printf 'proxy_health: report not stored: %s\n' "$(safe_log "$STORE_ERR")" >&2
      ;;
  esac
fi

if [ "$ALERT" = "true" ] && [ "$STATUS" != "healthy" ]; then
  if [ -z "$PROJECT" ]; then
    printf 'proxy_health: no project for the alert broadcast, YESMEM_PROJECT is unset\n' >&2
    exit 1
  fi
  BODY=$(printf '{"sender":"HealthChecker","project":"%s","content":"PROXY HEALTH [%s] (%s): %s"}' "$PROJECT" "$(echo "$STATUS" | tr a-z A-Z)" "$SINCE" "$SUMMARY")
  # The daemon answers one request and leaves the connection open, so a bare
  # "nc -U" waits for an EOF that never comes and the run dies on the job
  # timeout. -N shuts the write side after stdin, -w is the backstop. "--" ends
  # the options: YESMEM_SOCK is set by the daemon and a path starting with "-"
  # would otherwise be read as an nc option and the alert would never leave.
  # nc names the socket in its own diagnostics and that text is not ours, so its
  # stderr is dropped; the exit code rides in our own message instead.
  NC_EXIT=0
  REPLY=$(printf '{"method":"broadcast","params":%s}\n' "$BODY" | nc -U -N -w 10 -- "$SOCK" 2>/dev/null) || NC_EXIT=$?
  case "$REPLY" in
    *'"error"'*)
      printf 'proxy_health: broadcast rejected: %s\n' "$(safe_log "$REPLY")" >&2
      exit 1
      ;;
    *'"result"'*) ;;
    '')
      printf 'proxy_health: broadcast to %s got no answer (nc exit %s)\n' "$(safe_log "$SOCK")" "$NC_EXIT" >&2
      exit 1
      ;;
    *)
      printf 'proxy_health: broadcast to %s got an unexpected answer: %s\n' "$(safe_log "$SOCK")" "$(safe_log "$REPLY")" >&2
      exit 1
      ;;
  esac
fi

if [ "$STORE_FAILED" = "1" ]; then
  # Already reported above; only the exit code is still owed.
  exit 1
fi
```

## Database

```sql
CREATE TABLE cap_proxy_health__reports (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp TEXT,
  status TEXT,
  summary TEXT,
  issues_json TEXT,
  hours INTEGER,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```
