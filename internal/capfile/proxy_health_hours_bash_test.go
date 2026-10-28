package capfile

import (
	"encoding/json"
	"strings"
	"testing"
)

// The bash handler of the bundled proxy_health cap builds its journalctl window
// from HOURS, which the scheduled job passes in. It used to reject only
// non-digits: hours=0 asked for a window that ends now, any size reached the
// report JSON and six journal scans, and "0008" produced invalid JSON (YM-299).
// These tests run the real bash body with a stubbed journalctl and store(), and
// read the window back out of the report the script hands to store().
//
// The accepted range is 1..168 like the JS variant. A fraction falls back to one
// hour here, while the JS variant truncates it (8.5 becomes 8); both stay inside
// the range.
func TestLos38jProxyHealthBashClampsHours(t *testing.T) {
	cases := []struct {
		name   string
		hours  string
		locale string
		want   float64
	}{
		{name: "zero falls back", hours: "0", want: 1},
		{name: "above bound falls back", hours: "9999", want: 1},
		{name: "just above bound falls back", hours: "169", want: 1},
		{name: "upper bound", hours: "168", want: 168},
		{name: "numeric", hours: "3", want: 3},
		{name: "fraction falls back", hours: "8.5", want: 1},
		{name: "non-number falls back", hours: "abc", want: 1},
		{name: "empty falls back", hours: "", want: 1},
		// A leading zero is digits-only, so the old check let it through and the
		// report carried "hours":0008, which is not valid JSON.
		{name: "leading zero is normalized", hours: "0008", want: 8},
		// A glob range such as 0-9 follows the process collation, and under
		// de_DE.UTF-8 it also matches a non-ASCII digit, which then reached the
		// report as invalid JSON.
		{name: "non-ASCII digit under a European collation", hours: "٣", locale: "de_DE.UTF-8", want: 1},
		// The strip of leading zeros copies the remaining string per removed zero,
		// so an unbounded strip was quadratic in a caller-supplied value: 20000
		// zeros took 52s against the cap's 120s timeout. The strip is bounded now.
		{name: "many leading zeros", hours: strings.Repeat("0", 20000), want: 1},
		{name: "padded beyond the bound", hours: "000000000000008", want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"ALERT": "false",
				"HOURS": tc.hours,
			}
			if tc.locale != "" {
				env["LC_ALL"] = tc.locale
			}
			res := los32cRunBash(t, env)
			if res.exitCode != 0 {
				t.Fatalf("exit code %d, want 0 (stderr=%q)", res.exitCode, res.stderr)
			}
			if res.report == "" {
				t.Fatal("the handler stored no report, so the window cannot be read back")
			}
			var payload struct {
				Data struct {
					Hours float64 `json:"hours"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(res.report), &payload); err != nil {
				t.Fatalf("report is not valid JSON with HOURS=%q: %v\n%s", tc.hours, err, res.report)
			}
			if payload.Data.Hours != tc.want {
				t.Fatalf("HOURS=%q: report hours = %v, want %v", tc.hours, payload.Data.Hours, tc.want)
			}
		})
	}
}
