package proxy

import "testing"

func TestTokenCheckpoints_Due(t *testing.T) {
	const interval = 10000

	tests := []struct {
		name  string
		marks []int // successive totalTokens values for the same thread
		want  []bool
	}{
		{"first call only sets the mark", []int{5000}, []bool{false}},
		{"under the interval", []int{5000, 9000}, []bool{false, false}},
		{"at the interval", []int{5000, 15000}, []bool{false, true}},
		{"over the interval", []int{5000, 16000}, []bool{false, true}},
		{"mark moves forward after firing", []int{5000, 15000, 20000, 25000}, []bool{false, true, false, true}},
		{"shrink resets instead of going negative", []int{100000, 5000, 15000}, []bool{false, false, true}},
		{"shrink twice keeps resetting", []int{100000, 5000, 2000, 12000}, []bool{false, false, false, true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTokenCheckpoints()
			for i, tokens := range tt.marks {
				if got := c.due("tid", tokens, interval); got != tt.want[i] {
					t.Fatalf("due(%d) = %v, want %v (marks %v)", tokens, got, tt.want[i], tt.marks)
				}
			}
		})
	}
}

func TestTokenCheckpoints_ThreadsAreIndependent(t *testing.T) {
	c := newTokenCheckpoints()
	c.due("a", 100000, 10000)
	c.due("b", 5000, 10000)

	if !c.due("b", 15000, 10000) {
		t.Fatal("thread b should be due, its own mark is 5000")
	}
	if c.due("a", 15000, 10000) {
		t.Fatal("thread a shrank to 15000 from 100000 — not due")
	}
}

func TestTokenCheckpoints_ResetAndClear(t *testing.T) {
	c := newTokenCheckpoints()
	c.reset("tid", 42000)

	if got, ok := c.lastTokenCount["tid"]; !ok || got != 42000 {
		t.Fatalf("mark after reset = %d (present=%v), want 42000", got, ok)
	}

	c.clear("tid")

	if _, ok := c.lastTokenCount["tid"]; ok {
		t.Fatal("mark should be gone after clear")
	}
}
