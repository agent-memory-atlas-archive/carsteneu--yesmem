package capfile

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bundled telegram cap had three blind spots the audit caught (YM-368):
// telegram_poll read a curl failure through $? after an assignment that set -e
// already aborted on, telegram_send answered a refused delivery with exit 0
// because a bare curl does, and both poll and reply wrote a foreign message text
// and sender name into the job log without touching it, so a newline in a
// Telegram message wrote a line nobody can tell from ours.
//
// These tests run the real bodies the way fireCapHandler does: set -e first,
// then the store()/llm() polyfills, then the handler code. yesmem and curl are
// stubs on PATH, so no API, daemon, socket or database is reached and /tmp stays
// out of it. The assertions read what the run leaves behind — exit code, stdout,
// job log — not the shape of the script.
//
// The set -e prefix is the whole point: without it the poll's $? check works and
// the defect is invisible, which is why the earlier tests (they ran the bodies
// bare) never saw it.

type los38xResult struct {
	exitCode int
	stdout   string
	stderr   string
	// log is the job log the handler appends to (tsend.log, tpoll.log,
	// treply.log), empty when the run wrote none.
	log string
	// curl is the argv of every curl call, one per line. Only the form encoding
	// is asserted on it: whether a value arrives as a separate field is decided
	// by curl, not by the stub.
	curl string
}

// los38xYesmemStub answers the `yesmem json` calls of the three bodies. It is a
// fixture, not a jq: every value a test needs is handed in through the
// environment, and the two expressions that read a stored row are peeled off the
// stub input with sed, so the bodies still see what store() returned.
const los38xYesmemStub = `#!/bin/sh
expr=""
for a in "$@"; do expr="$a"; done
# LOS38X_JSON_FAIL names an expression the stub answers with exit non-zero, the
# way yesmem json answers a body that is not JSON at all (a proxy error page).
if [ -n "${LOS38X_JSON_FAIL-}" ] && printf '%s' "$expr" | grep -qF "$LOS38X_JSON_FAIL"; then
  exit "${LOS38X_JSON_EXIT:-4}"
fi
input=$(cat)
# LOS38X_JSON_FAIL_INPUT fails only the calls whose stdin carries a marker, so a
# test can break one update of a batch instead of all of them.
if [ -n "${LOS38X_JSON_FAIL_INPUT-}" ] && printf '%s' "$input" | grep -qF "$LOS38X_JSON_FAIL_INPUT"; then
  exit "${LOS38X_JSON_EXIT:-4}"
fi
case "$expr" in
  *'rows[0].value'*)
    printf '%s\n' "$input" | sed -n 's/.*"value":"\([^"]*\)".*/\1/p' | head -n1 ;;
  *'rows[0].name'*) printf '%s\n' "${LOS38X_ROW_NAME-}" ;;
  *'rows[0].session_id'*|*'rows[0].last_used_at'*)
    printf '\n' ;;
  '.claimed')    printf 'true\n' ;;
  '.row.id')     printf '1\n' ;;
  '.row.text')   printf '%s' "${LOS38X_CLAIM_TEXT-}" ;;
  '.row.sender') printf '%s' "${LOS38X_CLAIM_SENDER-}" ;;
  '.result | length') printf '%s\n' "${LOS38X_COUNT:-0}" ;;
  '.result[0]')  printf '%s' "${LOS38X_UPDATE-}" ;;
  '.result[1]')  printf '%s' "${LOS38X_UPDATE2-}" ;;
  '.update_id')  printf '%s\n' "$input" | sed -n 's/.*"update_id":\([0-9]*\).*/\1/p' | head -n1 ;;
  *'first_name'*) printf '%s' "${LOS38X_FIRSTNAME-}" ;;
  *'capability'*) printf '{}\n' ;;
  '.result // empty') printf '%s' "${LOS38X_REPLY-}" ;;
  '.session_id // empty') printf '\n' ;;
  '.ok // false')
    if printf '%s\n' "$input" | grep -q '"ok":true'; then printf 'true\n'; else printf 'false\n'; fi ;;
  '.description // empty')
    if [ -n "${LOS38X_DESCRIPTION-}" ]; then printf '%s' "$LOS38X_DESCRIPTION"; else
      printf '%s\n' "$input" | sed -n 's/.*"description":"\([^"]*\)".*/\1/p' | head -n1
    fi ;;
  '.') exit 0 ;;
  *) printf '\n' ;;
esac
exit 0
`

// los38xRun starts one handler body under set -e with the polyfills in front.
func los38xRun(t *testing.T, script string, env map[string]string) los38xResult {
	return los38xRunWithFiles(t, script, env, nil)
}

// los38xRunWithFiles is los38xRun with files written below the run's own HOME, so
// a body that reads a config path relative to HOME can be driven without touching
// the real one.
func los38xRunWithFiles(t *testing.T, script string, env map[string]string, files map[string]string) los38xResult {
	t.Helper()
	dir := t.TempDir()

	for name, body := range files {
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeStub(t, filepath.Join(bin, "yesmem"), los38xYesmemStub)
	curlCalls := filepath.Join(dir, "curl.calls")
	writeStub(t, filepath.Join(bin, "curl"), `#!/bin/sh
printf '%s\n' "$*" >> "$LOS38X_CURL_CALLS"
if [ -n "${LOS38X_CURL_BODY-}" ]; then printf '%s' "$LOS38X_CURL_BODY"; fi
exit "${LOS38X_CURL_EXIT:-0}"
`)

	store := filepath.Join(dir, "store.log")
	scriptPath := filepath.Join(dir, "run.sh")
	prelude := `set -e
store() {
  printf '%s\n' "$1" >> "$LOS38X_STORE"
  case "$1" in
    *claim_and_read*) printf '{"claimed":true}\n' ;;
    *bot_token*) printf '{"rows":[{"value":"TESTTOKEN"}]}\n' ;;
    *chat_id*) printf '{"rows":[{"value":"4242"}]}\n' ;;
    *offset*) printf '{"rows":[{"value":"0"}]}\n' ;;
    *) printf '{"rows":[]}\n' ;;
  esac
  return 0
}
llm() { printf '%s' "${LOS38X_LLM_RESULT-}"; return "${LOS38X_LLM_EXIT:-0}"; }
`
	if err := os.WriteFile(scriptPath, []byte(prelude+bundledScript(t, telegramCapPath, script)), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		// A temp HOME and job dir keep the default socket path and the real
		// job logs away from this run.
		"HOME="+dir,
		"YESMEM_JOB_DIR="+dir,
		"YESMEM_SOCK="+filepath.Join(dir, "job.sock"),
		"LOS38X_STORE="+store,
		"LOS38X_CURL_CALLS="+curlCalls,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := los38xResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run handler: %v", err)
		}
		res.exitCode = exitErr.ExitCode()
	}
	for _, name := range []string{"tsend.log", "tpoll.log", "treply.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		res.log += string(data)
	}
	if data, err := os.ReadFile(curlCalls); err == nil {
		res.curl = string(data)
	}
	return res
}

// A bare curl reports an HTTP refusal as a delivered message: it exits 0 and
// writes the API's error body to stdout. telegram_send handed that back as its
// result, so the caller believed the message went out. The .ok check turns the
// refusal into a failed send, and the API's own sentence reaches the caller: it
// is the only diagnosis there is.
func TestLos38xTelegramSendReportsRefusedDelivery(t *testing.T) {
	requireLinuxCapHost(t)
	cases := []struct {
		name       string
		body       string
		curlExit   string
		wantFailed bool
		// wantReason is the part of the API answer the caller has to see,
		// because nothing else names the cause.
		wantReason string
		// description overrides what the stub returns for .description, so a
		// case can hand in text that the JSON body could not carry verbatim.
		description string
	}{
		{
			// A 4xx answer without -f: curl exits 0 and prints the body.
			name:       "http refusal with the api answer",
			body:       `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			curlExit:   "0",
			wantFailed: true,
			wantReason: "chat not found",
		},
		{
			// The API's own sentence ends up in the log line, so a line break
			// in it must not start a second one.
			name:        "refusal with a broken reason",
			body:        `{"ok":false,"error_code":400}`,
			description: "Bad Request\nOkt 18 15:00:03 host yesmem[1]: INJECTED-REASON",
			curlExit:    "0",
			wantFailed:  true,
			wantReason:  "INJECTED-REASON",
		},
		{
			// A transport failure has no body at all; the exit code is all the
			// caller gets.
			name:       "transport failure",
			curlExit:   "28",
			wantFailed: true,
			wantReason: "status=28",
		},
		{
			name:     "delivered",
			body:     `{"ok":true,"result":{"message_id":7}}`,
			curlExit: "0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := los38xRun(t, "telegram_send", map[string]string{
				"TEXT":               "hallo",
				"LOS38X_CURL_BODY":   tc.body,
				"LOS38X_CURL_EXIT":   tc.curlExit,
				"LOS38X_DESCRIPTION": tc.description,
			})

			if tc.wantFailed {
				if res.exitCode == 0 {
					t.Fatalf("exit code 0 after the API refused the message (stdout=%q, log=%q); the caller believes it was delivered",
						strings.TrimSpace(res.stdout), res.log)
				}
				if !strings.Contains(res.log, "sendMessage failed") {
					t.Errorf("log = %q, want a line that names the failed send", res.log)
				}
				if !strings.Contains(res.stdout, tc.wantReason) {
					t.Errorf("stdout = %q, want it to carry %q so the caller knows why the send failed",
						strings.TrimSpace(res.stdout), tc.wantReason)
				}
				for _, line := range strings.Split(res.log, "\n") {
					if strings.Contains(line, "INJECTED-") && !strings.Contains(line, "sendMessage failed") {
						t.Errorf("the api reason wrote its own log line: %q", line)
					}
				}
				return
			}
			if res.exitCode != 0 {
				t.Fatalf("exit code %d for a delivered message (stderr=%q, log=%q)", res.exitCode, res.stderr, res.log)
			}
			if !strings.Contains(res.stdout, "message_id") {
				t.Errorf("stdout = %q, want the API answer of a successful send", res.stdout)
			}
			if strings.Contains(res.log, "sendMessage failed") {
				t.Errorf("log = %q, want no failure line for a delivered message", res.log)
			}
		})
	}
}

// The message text is a form value, and -d sends it as it is: an "&" in it
// starts another field. The reply bodies already encode their text; the send
// has to do the same (measured 20.10.2026 against a local listener).
func TestLos38xTelegramSendEncodesTheText(t *testing.T) {
	res := los38xRun(t, "telegram_send", map[string]string{
		"TEXT":             "hi&chat_id=666&parse_mode=HTML",
		"LOS38X_CURL_BODY": `{"ok":true,"result":{"message_id":7}}`,
	})

	if !strings.Contains(res.curl, "--data-urlencode") {
		t.Errorf("curl called as %q, want the text urlencoded so it stays one field", strings.TrimSpace(res.curl))
	}
	if strings.Contains(res.curl, "-d text=") || strings.Contains(res.curl, " -d \"text=") {
		t.Errorf("curl called as %q, want no raw -d for the message text", strings.TrimSpace(res.curl))
	}
}

// telegram_poll assigned curl's output and read the status on the next line. The
// assignment takes the status of the substitution, so set -e ended the run
// before $? was read: the job was counted as failed, and the handler's own
// branch, a logged step with exit 0, was dead code. A transport failure is a
// logged step, not an abort.
func TestLos38xTelegramPollSurvivesATransportFailure(t *testing.T) {
	res := los38xRun(t, "telegram_poll", map[string]string{
		"LOS38X_CURL_EXIT": "28",
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d under set -e after a curl timeout, want 0; the run ended before it logged anything (stderr=%q, log=%q)",
			res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "poll: curl failed exit=28") {
		t.Errorf("log = %q, want the failed poll line so the next reader sees why nothing arrived", res.log)
	}
}

// A sender name comes from Telegram, so it is foreign text. Written into the job
// log as it is, a newline in it adds a line the reader takes for ours.
func TestLos38xTelegramPollNeutralisesForeignSender(t *testing.T) {
	requireLinuxCapHost(t)
	const injected = "Okt 18 15:00:00 host yesmem[1]: INJECTED-POLL"
	res := los38xRun(t, "telegram_poll", map[string]string{
		"LOS38X_COUNT":     "1",
		"LOS38X_UPDATE":    `{"update_id":7,"message":{"chat":{"id":42},"from":{"first_name":"Jan\n` + injected + `"},"text":"hi","date":1}}`,
		"LOS38X_FIRSTNAME": "Jan\n" + injected,
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for one stored update (stderr=%q, log=%q)", res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "stored update_id=7") {
		t.Fatalf("log = %q, want the stored-update line the sender name lands in", res.log)
	}
	for _, line := range strings.Split(res.log, "\n") {
		if !strings.Contains(line, "INJECTED-POLL") {
			continue
		}
		if !strings.Contains(line, "stored update_id=7") {
			t.Errorf("a foreign sender name wrote its own log line: %q", line)
		}
	}
	// Dropping the name entirely would pass the check above, so the excerpt has
	// to keep the readable part on the line it belongs to.
	if !strings.Contains(res.log, "from Jan Okt 18") {
		t.Errorf("log = %q, want the sender name kept on the stored-update line", res.log)
	}
}

// The reply handler logs the claimed message text and sender before it decides
// anything, and the model's answer after it replied. All three are foreign text.
func TestLos38xTelegramReplyNeutralisesForeignText(t *testing.T) {
	requireLinuxCapHost(t)
	res := los38xRun(t, "telegram_reply", map[string]string{
		"LOS38X_CLAIM_TEXT":   "hallo\nOkt 18 15:00:00 host yesmem[1]: INJECTED-TEXT\u009b31mX",
		"LOS38X_CLAIM_SENDER": "Jan\nOkt 18 15:00:01 host yesmem[1]: INJECTED-SENDER",
		// The envelope is what the llm polyfill returns; LOS38X_REPLY is the
		// value jq's .result yields, with the escape already decoded — that is
		// the string the handler logs.
		"LOS38X_LLM_RESULT": `{"result":"ok\nOkt 18 15:00:02 host yesmem[1]: INJECTED-REPLY","session_id":""}`,
		"LOS38X_REPLY":      "ok\nOkt 18 15:00:02 host yesmem[1]: INJECTED-REPLY",
		"LOS38X_CURL_BODY":  `{"ok":true,"result":{"message_id":9}}`,
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for a delivered reply (stderr=%q, log=%q)", res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "reply: replying row=1") {
		t.Fatalf("log = %q, want the claim line the message text lands in", res.log)
	}
	if !strings.Contains(res.log, "reply: sent row=1") {
		t.Fatalf("log = %q, want the sent line the model answer lands in", res.log)
	}
	for _, line := range strings.Split(res.log, "\n") {
		if !strings.Contains(line, "INJECTED-") {
			continue
		}
		if !strings.Contains(line, "reply:") {
			t.Errorf("foreign text wrote its own log line: %q", line)
		}
	}
	// Dropping the values entirely would pass the checks above, so the excerpt
	// has to keep the readable part on the line it belongs to.
	for _, want := range []string{"sender=Jan Okt 18", "text=hallo Okt 18", "> ok Okt 18"} {
		if !strings.Contains(res.log, want) {
			t.Errorf("log = %q, want it to carry %q", res.log, want)
		}
	}
	// U+009B is the one-byte form of CSI and reaches the handler as the two-byte
	// sequence C2 9B, which a C0-only strip leaves alone.
	if strings.Contains(res.log, "\u009b") {
		t.Errorf("log = %q, want the C1 control character gone", res.log)
	}
}
