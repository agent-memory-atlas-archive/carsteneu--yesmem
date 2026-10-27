package daemon

import "fmt"

type SandboxProfile int

const (
	ProfileNone SandboxProfile = iota
	ProfileStandard
	ProfileStrict
)

func (p SandboxProfile) String() string {
	switch p {
	case ProfileNone:
		return "none"
	case ProfileStandard:
		return "standard"
	case ProfileStrict:
		return "strict"
	default:
		return "none"
	}
}

func ParseSandboxProfile(s string) (SandboxProfile, error) {
	switch s {
	case "none":
		return ProfileNone, nil
	case "standard":
		return ProfileStandard, nil
	case "strict":
		return ProfileStrict, nil
	case "":
		// An omitted profile is not a decision: it inherits (see
		// resolveScriptSandbox / the job profile). Mapping it to none here is
		// what made every cap without a sandbox line run unsandboxed (YM-230).
		// The error path returns standard, so a caller that ignores the error
		// lands on the sandboxed side.
		return ProfileStandard, fmt.Errorf("empty sandbox profile: omit the field to inherit, or name none explicitly")
	default:
		return ProfileStandard, fmt.Errorf("unknown sandbox profile: %q (valid: none, standard, strict)", s)
	}
}

// configuredDefaultSandbox is the profile a script inherits when it names none:
// default_sandbox_profile from config.yaml, standard when the config leaves it
// open. A configured none is a decision and is honored (flag, because none is
// the zero value).
func (h *Handler) configuredDefaultSandbox() SandboxProfile {
	if h.sandboxDefaultSet {
		return h.defaultSandboxProfile
	}
	return ProfileStandard
}

// resolveScriptSandboxWith resolves a script's effective profile against a given
// configured default: the script's own profile wins, an absent one inherits.
func resolveScriptSandboxWith(scriptSandbox string, defaultProfile SandboxProfile) (SandboxProfile, error) {
	if scriptSandbox == "" {
		return defaultProfile, nil
	}
	return ParseSandboxProfile(scriptSandbox)
}

// resolveScriptSandbox resolves the profile a cap script runs under outside a
// scheduled job: the script's own profile wins, an absent one inherits the
// configured default. Inside a job the job profile sits between the two; the
// scheduler resolves that chain in fireJobBash.
func (h *Handler) resolveScriptSandbox(scriptSandbox string) (SandboxProfile, error) {
	return resolveScriptSandboxWith(scriptSandbox, h.configuredDefaultSandbox())
}
