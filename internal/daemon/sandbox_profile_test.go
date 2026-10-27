package daemon

import (
	"strings"
	"testing"
)

func TestParseSandboxProfile(t *testing.T) {
	tests := []struct {
		input string
		want  SandboxProfile
		ok    bool
	}{
		{"none", ProfileNone, true},
		{"standard", ProfileStandard, true},
		{"strict", ProfileStrict, true},
		// YM-230: an empty value is not a decision. It used to parse to none,
		// which made every cap without a sandbox line run unsandboxed.
		{"", ProfileStandard, false},
		{"danger-full-access", ProfileStandard, false},
		{"STANDARD", ProfileStandard, false},
	}
	for _, tt := range tests {
		got, err := ParseSandboxProfile(tt.input)
		if tt.ok && err != nil {
			t.Errorf("ParseSandboxProfile(%q) unexpected error: %v", tt.input, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("ParseSandboxProfile(%q) expected error, got %v", tt.input, got)
		}
		if tt.ok && got != tt.want {
			t.Errorf("ParseSandboxProfile(%q) = %v, want %v", tt.input, got, tt.want)
		}
		// The error path must land on the sandboxed side: a caller that drops the
		// error would otherwise run unsandboxed (YM-230).
		if !tt.ok && got != ProfileStandard {
			t.Errorf("ParseSandboxProfile(%q) = %v on the error path, want standard", tt.input, got)
		}
	}
}

func TestSandboxRunWithProfile_None(t *testing.T) {
	s := NewSandbox(SandboxConfig{AllowedPorts: []int{80, 443}})
	output, exitCode, err := s.RunWithProfile("echo sandboxtest", 5, ProfileNone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(output, "sandboxtest") {
		t.Errorf("output = %q, want to contain 'sandboxtest'", output)
	}
}

func TestSandboxWrapExecArgs(t *testing.T) {
	s := &Sandbox{
		binaryPath: "/usr/local/bin/ai-jail",
		cfg:        SandboxConfig{AllowedPorts: []int{80, 443}},
	}
	bin, args, err := s.WrapExecArgs("claude", []string{"-p", "--output-format", "json"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if bin != "/usr/local/bin/ai-jail" {
		t.Errorf("binary = %q, want ai-jail path", bin)
	}
	if len(args) < 5 {
		t.Fatalf("expected at least 5 args, got %d: %v", len(args), args)
	}
	found := false
	for _, a := range args {
		if a == "claude" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected claude in args, got %v", args)
	}
}

func TestSandboxWrapExecArgs_None(t *testing.T) {
	s := &Sandbox{
		binaryPath: "/usr/local/bin/ai-jail",
		cfg:        SandboxConfig{AllowedPorts: []int{80, 443}},
	}
	bin, args, err := s.WrapExecArgs("claude", []string{"-p", "--verbose"}, ProfileNone)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if bin != "claude" {
		t.Errorf("binary = %q, want claude (no wrapping)", bin)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(args), args)
	}
}

func TestSandboxProfileString(t *testing.T) {
	tests := []struct {
		profile SandboxProfile
		want    string
	}{
		{ProfileNone, "none"},
		{ProfileStandard, "standard"},
		{ProfileStrict, "strict"},
		{SandboxProfile(99), "none"},
	}
	for _, tt := range tests {
		if got := tt.profile.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.profile, got, tt.want)
		}
	}
}
