package proxy

import "testing"

// YM-396 (PRX-P49), Nacharbeit zu Los 55c: CacheStatusWriter startet im
// Konstruktor einen Ticker und hatte kein Stop. Jeder Testkonstruktor, der einen
// Writer aufbaut, liess damit eine Goroutine fuer den Rest des Testprozesses
// laufen; die tickende Schleife allokiert rund 19 Objekte pro Tick und landet
// damit in jedem testing.AllocsPerRun-Fenster, das zufaellig in einen Tick
// faellt (belegt: eine Closure, die nur schlaeft, zaehlt mit lebendem Writer
// 19 Allokationen und ohne Writer 0).
//
// Dieser Test laeuft als letzter im Paket (Dateiname zzz_) und haelt den Befund
// dauerhaft fest. Die Obergrenze ist 0 und keine Toleranz: seit YM-399 beenden
// auch die beiden Run-Harness-Tests ihren Server (openai_integration_test.go,
// openai_dump_integration_test.go), statt ihn stehen zu lassen.
func TestZZZLos55cNoTickerGoroutinesAreLeftBehind(t *testing.T) {
	if live := liveCacheStatusWriters.Load(); live != 0 {
		t.Fatalf("%d CacheStatusWriter ticker loops outlive the tests that built them: a writer built directly needs t.Cleanup(w.Stop), and a proxy started with runUntil must be stopped before the test ends", live)
	}
}
