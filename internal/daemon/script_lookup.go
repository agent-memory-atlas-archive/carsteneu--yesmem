package daemon

func findBashScript(meta *CapMeta, scriptName string) (string, string, bool) {
	sc, ok := findBashScriptMeta(meta, scriptName)
	if !ok {
		return "", "", false
	}
	return sc.Body, sc.Sandbox, true
}

// findBashScriptMeta is findBashScript with the full script metadata, so callers
// that build a sandbox get the script's own egress allowlist too.
func findBashScriptMeta(meta *CapMeta, scriptName string) (ScriptMeta, bool) {
	if meta == nil {
		return ScriptMeta{}, false
	}
	if scriptName != "" {
		for _, sc := range meta.Scripts {
			if sc.Name == scriptName && sc.Runtime == "bash" {
				return sc, true
			}
		}
		return ScriptMeta{}, false
	}
	for _, sc := range meta.Scripts {
		if sc.Runtime == "bash" && sc.Kind == "handler" {
			return sc, true
		}
	}
	for _, sc := range meta.Scripts {
		if sc.Runtime == "bash" {
			return sc, true
		}
	}
	return ScriptMeta{}, false
}
