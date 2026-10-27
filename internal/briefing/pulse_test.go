package briefing

import "testing"

// YM-102: the narrative prompt emits "Pulse: ...", the extractor looked for
// "Puls:" only, so the pulse section was always empty. Both spellings must work.
func TestExtractPulse_AcceptsBothSpellings(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "english prefix from the prompt",
			content: "I'm back. We fixed the scanner.\n\nPulse: high · focused · Next: verify backfill",
			want:    "Pulse: high · focused · Next: verify backfill",
		},
		{
			name:    "german prefix still accepted",
			content: "Ich bin zurück.\n\nPuls: hoch · fokussiert · Nächstes: Backfill prüfen",
			want:    "Puls: hoch · fokussiert · Nächstes: Backfill prüfen",
		},
		{
			name:    "no pulse line",
			content: "I'm back.\n\nOnly prose here.",
			want:    "",
		},
		{
			name:    "pulse after prose with leading whitespace",
			content: "Text\n   Pulse: low · calm · Next: wait",
			want:    "Pulse: low · calm · Next: wait",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractPulse(tt.content); got != tt.want {
				t.Errorf("extractPulse() = %q, want %q", got, tt.want)
			}
		})
	}
}
