package archive

import (
	"strings"
	"testing"
)

// YM-357 (Los 11k): der Encoder zaehlte Runen, Claude Code zaehlt UTF-16-Codeunits.
//
// Der Sollwert stammt aus der ausgelieferten CLI selbst (2.1.301, Byte
// 216639673): `O=200;function N(t){let e=t.replace(/[^a-zA-Z0-9]/g,"-");
// if(e.length<=O)return e;return `${e.slice(0,O)}-${Math.abs(Kle(t)).toString(36)}`}`.
// Das Regex traegt kein "u"-Flag, also ersetzt es jede Codeunit einzeln: ein
// Nicht-BMP-Zeichen wird zu zwei Strichen. e.length und slice zaehlen ebenfalls
// Codeunits, die 200er-Grenze liegt damit zwei Einheiten vor der Runenzahl.
func TestProjectDirNameCountsUTF16CodeUnits_Los11k(t *testing.T) {
	cases := []struct{ name, path, want string }{
		{
			// Ein Emoji ist in UTF-16 ein Surrogatpaar, also zwei Striche statt
			// einem. Der bisherige Encoder lieferte "-home--" (Rune).
			name: "non-BMP character becomes two dashes",
			path: "/home/\U0001F600",
			want: "-home---",
		},
		{
			// Genau 200 Codeunits: 197 a plus Surrogatpaar. Kein Hash, die Grenze
			// ist in Codeunits erreicht und nicht in Runen (das waeren 199).
			name: "exactly 200 code units is not truncated",
			path: "/" + strings.Repeat("a", 197) + "\U0001F600",
			want: "-" + strings.Repeat("a", 197) + "--",
		},
		{
			// 201 Codeunits: der Schnitt faellt zwischen die beiden Striche des
			// Paares, danach folgt der Hash des Pfades (base36).
			name: "201 code units is cut at the unit boundary and hashed",
			path: "/" + strings.Repeat("a", 198) + "\U0001F600",
			want: "-" + strings.Repeat("a", 198) + "--b5wpam",
		},
		{
			// Ungueltiges UTF-8 wird zu U+FFFD und damit zu einem Strich pro
			// dekodierter Rune. Bei einer abgeschnittenen Mehrbyte-Sequenz zaehlt
			// Node einen Ersatzpunkt, Go deren zwei: die Strichzahl weicht dann ab,
			// in derselben Groessenordnung wie vor der UTF-16-Umstellung.
			name: "invalid UTF-8 becomes a replacement rune per decoded rune",
			path: "/home/\xff",
			want: "-home--",
		},
	}
	for _, tc := range cases {
		if got := ProjectDirName(tc.path); got != tc.want {
			t.Errorf("%s: ProjectDirName(%q) = %q (len %d), want %q (len %d)",
				tc.name, tc.path, got, len(got), tc.want, len(tc.want))
		}
	}
}
