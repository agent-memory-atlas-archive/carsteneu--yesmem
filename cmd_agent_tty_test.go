package main

import (
	"errors"
	"flag"
	"testing"
)

func TestParseAgentTTYFlags(t *testing.T) {
	o, err := parseAgentTTYFlags([]string{"--sock", "/run/a.sock", "--parent-pid", "1706879", "--parent-window", "0x04c00004"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.sock != "/run/a.sock" || o.parentPid != 1706879 || o.parentWindow != "0x04c00004" {
		t.Fatalf("got %+v", o)
	}
}

func TestParseAgentTTYFlags_PlacementOptional(t *testing.T) {
	o, err := parseAgentTTYFlags([]string{"--sock", "/run/a.sock"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.parentPid != 0 || o.parentWindow != "" {
		t.Fatalf("placement must default to off, got %+v", o)
	}
	if o.wantsPlacement() {
		t.Fatal("no caller pid and no caller window: nothing to inherit")
	}
}

func TestAgentTTYOpts_WantsPlacement(t *testing.T) {
	if !(agentTTYOpts{parentPid: 900}).wantsPlacement() {
		t.Error("caller pid alone must trigger placement")
	}
	if !(agentTTYOpts{parentWindow: "0x04c00004"}).wantsPlacement() {
		t.Error("caller window alone must trigger placement (shared opencode serve)")
	}
}

func TestParseAgentTTYFlags_RequiresSock(t *testing.T) {
	if _, err := parseAgentTTYFlags([]string{"--parent-pid", "900"}); err == nil {
		t.Fatal("missing --sock must be an error")
	}
}

// -h must stay a clean, successful exit as with the former ExitOnError set.
func TestParseAgentTTYFlags_Help(t *testing.T) {
	if _, err := parseAgentTTYFlags([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("want flag.ErrHelp, got %v", err)
	}
}

// Exit codes match the former flag.ExitOnError behaviour: 0 for -h, 2 for a
// bad flag, 1 for a missing --sock.
func TestAgentTTYExitCode(t *testing.T) {
	_, help := parseAgentTTYFlags([]string{"-h"})
	_, bad := parseAgentTTYFlags([]string{"--bogus"})
	_, nosock := parseAgentTTYFlags([]string{})
	for _, c := range []struct {
		err  error
		want int
	}{{help, 0}, {bad, 2}, {nosock, 1}} {
		if got := agentTTYExitCode(c.err); got != c.want {
			t.Errorf("agentTTYExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
