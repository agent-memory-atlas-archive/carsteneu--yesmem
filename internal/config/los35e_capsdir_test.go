package config

import (
	"path/filepath"
	"testing"
)

// YM-353 (OPS-87, key 3): the caps dir was derived in four places, and the
// installer derived it from the home directory while the daemon derived it from
// the data dir. This is the single derivation both sides share now.
func TestLos35eCapsDirPath(t *testing.T) {
	home := t.TempDir()
	stdData := filepath.Join(home, ".claude", "yesmem")

	cases := []struct {
		name       string
		configured string
		dataDir    string
		want       string
	}{
		{
			name:    "standard install derives the sibling of the data dir",
			dataDir: stdData,
			want:    filepath.Join(home, ".claude", "caps"),
		},
		{
			name:    "a data dir outside the claude home moves the caps dir with it",
			dataDir: filepath.Join(home, "xdg-data", "yesmem"),
			want:    filepath.Join(home, "xdg-data", "caps"),
		},
		{
			name:       "a configured caps_dir wins",
			configured: filepath.Join(home, "caps-here"),
			dataDir:    stdData,
			want:       filepath.Join(home, "caps-here"),
		},
		{
			name:       "a configured caps_dir wins without a data dir",
			configured: filepath.Join(home, "caps-here"),
			want:       filepath.Join(home, "caps-here"),
		},
		{
			name: "no data dir and no config yields nothing",
			want: "",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := CapsDirPath(tt.configured, tt.dataDir); got != tt.want {
				t.Errorf("CapsDirPath(%q, %q) = %q, want %q", tt.configured, tt.dataDir, got, tt.want)
			}
		})
	}
}
