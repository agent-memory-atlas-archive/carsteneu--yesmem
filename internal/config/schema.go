package config

import (
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The key space is derived from the Config struct instead of a hand-kept list:
// a list drifts as soon as a field is added, and a drifted list is exactly the
// failure mode this guards against. A key nobody rejects is a key nobody reads.

var (
	configType = reflect.TypeOf(Config{})
	proxyType  = reflect.TypeOf(ProxyConfig{})
)

// yamlField returns the type of the field carrying the given yaml tag.
func yamlField(t reflect.Type, name string) (reflect.Type, bool) {
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return nil, false
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if tag == name {
			return f.Type, true
		}
	}
	return nil, false
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// NormalizeSetPath maps a user-supplied dot-path onto its location in the
// schema. A known proxy key given without the "proxy." prefix moves under
// proxy:, because that is where Load reads it; a bare token_threshold at the
// top level is inert while the runtime override still applies, so YAML and
// runtime drift apart. A key the schema does not know is rejected instead of
// being written where no code looks.
func NormalizeSetPath(dotPath string) (string, error) {
	dotPath = strings.TrimSpace(dotPath)
	if dotPath == "" {
		return "", fmt.Errorf("empty config key")
	}

	// token_threshold:<model> is the per-model override documented since
	// 4903cfa4. Its home in the file is proxy.token_thresholds.<model>, so that
	// every reader and writer sees the same value and not only the runtime slot.
	for _, prefix := range []string{"token_threshold:", "proxy.token_threshold:"} {
		if model, ok := strings.CutPrefix(dotPath, prefix); ok && model != "" {
			dotPath = "proxy.token_thresholds." + model
			break
		}
	}

	top := dotPath
	if i := strings.Index(dotPath, "."); i >= 0 {
		top = dotPath[:i]
	}

	full := dotPath
	if _, ok := yamlField(configType, top); !ok {
		if _, isProxy := yamlField(proxyType, top); !isProxy {
			return "", fmt.Errorf("unknown config key %q: not a config section and not a proxy key", dotPath)
		}
		full = "proxy." + dotPath
	}

	if err := validateSetPath(full); err != nil {
		return "", err
	}
	return full, nil
}

// validateSetPath walks the schema along a dot-path and returns the type the
// path points at. Struct levels must name a field, map levels accept any key,
// and a scalar ends the path.
func validateSetPath(dotPath string) error {
	_, err := typeAtPath(dotPath)
	return err
}

// pathSegments splits a dot-path into the segments the document actually uses.
// A map key may contain dots ("glm-5.2"), so a naive split on "." would create
// nested keys and corrupt the file. The schema decides where the key ends: a
// field name never contains a dot, so the shortest key whose remainder still
// validates wins.
// maxPathSegments bounds the work pathSegments may do on one key. The schema is
// five levels deep; the limit only exists because a caller-supplied key is
// foreign input and the map backtracking is quadratic in the segment count.
const maxPathSegments = 16

// maxKeyLength bounds the key before it is split or echoed.
const maxKeyLength = 512

func pathSegments(dotPath string) ([]string, error) {
	// Checked before the split so an oversized key is not echoed back in the
	// error message.
	if len(dotPath) > maxKeyLength {
		return nil, fmt.Errorf("config key is %d bytes long, at most %d are allowed", len(dotPath), maxKeyLength)
	}
	all := strings.Split(dotPath, ".")
	if len(all) > maxPathSegments {
		return nil, fmt.Errorf("config key has %d segments, at most %d are allowed", len(all), maxPathSegments)
	}
	for _, part := range all {
		if part == "" {
			return nil, fmt.Errorf("config key %q has an empty segment", dotPath)
		}
	}
	return segmentParts(configType, all, all, 0)
}

func segmentParts(t reflect.Type, parts, all []string, off int) ([]string, error) {
	t = derefType(t)
	if len(parts) == 0 {
		return nil, nil
	}
	switch t.Kind() {
	case reflect.Struct:
		ft, ok := yamlField(t, parts[0])
		if !ok {
			return nil, fmt.Errorf("unknown config key %q: %s has no field %q", strings.Join(all, "."), pathLabel(all, off), parts[0])
		}
		rest, err := segmentParts(ft, parts[1:], all, off+1)
		if err != nil {
			return nil, err
		}
		return append([]string{parts[0]}, rest...), nil
	case reflect.Map:
		elem := derefType(t.Elem())
		if elem.Kind() != reflect.Struct && elem.Kind() != reflect.Map {
			// Scalar values: everything left is the map key, and a key may
			// itself contain dots.
			return []string{strings.Join(parts, ".")}, nil
		}
		for k := 1; k < len(parts); k++ {
			if rest, err := segmentParts(elem, parts[k:], all, off+k); err == nil {
				return append([]string{strings.Join(parts[:k], ".")}, rest...), nil
			}
		}
		// A path that ends on the map entry itself names a whole section
		// (model_features.deepseek, pricing.glm-5.2). Reading it is fine; writing
		// it is refused later, because a string cannot fill a section.
		if len(parts) > 0 {
			return []string{strings.Join(parts, ".")}, nil
		}
	}
	return nil, fmt.Errorf("unknown config key %q: %s is a value, not a section", strings.Join(all, "."), pathLabel(all, off))
}

// typeAtPath returns the schema type at dotPath.
func typeAtPath(dotPath string) (reflect.Type, error) {
	segments, err := pathSegments(dotPath)
	if err != nil {
		return nil, err
	}
	t := configType
	for _, segment := range segments {
		t = derefType(t)
		switch t.Kind() {
		case reflect.Struct:
			ft, ok := yamlField(t, segment)
			if !ok {
				return nil, fmt.Errorf("unknown config key %q: %q has no field %q", dotPath, segment, segment)
			}
			t = ft
		case reflect.Map:
			t = t.Elem()
		}
	}
	return derefType(t), nil
}

// CoerceValueForPath checks value against the schema field at dotPath and
// returns it in the field's type. Without this a string written into a typed
// field ("opus=500000" into token_threshold) makes the whole file fail to
// decode, and Load then falls back to defaults: one bad value would cost the
// entire running configuration.
func CoerceValueForPath(dotPath string, value any) (any, error) {
	t, err := typeAtPath(dotPath)
	if err != nil {
		return nil, err
	}
	switch t.Kind() {
	case reflect.String:
		if s, ok := value.(string); ok {
			return s, nil
		}
		return fmt.Sprint(value), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch v := value.(type) {
		case int:
			if v < 0 {
				return nil, fmt.Errorf("config key %q expects a whole number that is not negative, got %v", dotPath, value)
			}
			return v, nil
		case int64:
			if v < 0 {
				return nil, fmt.Errorf("config key %q expects a whole number that is not negative, got %v", dotPath, value)
			}
			return int(v), nil
		case float64:
			if v == float64(int(v)) && v >= 0 {
				return int(v), nil
			}
		case string:
			if i, convErr := strconv.Atoi(strings.TrimSpace(v)); convErr == nil && i >= 0 {
				return i, nil
			}
		}
		return nil, fmt.Errorf("config key %q expects a whole number that is not negative, got %v", dotPath, value)
	case reflect.Float32, reflect.Float64:
		// A NaN or an infinite price makes the budget arithmetic meaningless: a
		// NaN spend passes Check and blocks HasBudget at the same time.
		switch v := value.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return nil, fmt.Errorf("config key %q expects a finite number that is not negative, got %v", dotPath, value)
			}
			return v, nil
		case int:
			if v < 0 {
				return nil, fmt.Errorf("config key %q expects a finite number that is not negative, got %v", dotPath, value)
			}
			return float64(v), nil
		case int64:
			if v < 0 {
				return nil, fmt.Errorf("config key %q expects a finite number that is not negative, got %v", dotPath, value)
			}
			return float64(v), nil
		case string:
			f, convErr := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if convErr == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 {
				return f, nil
			}
		}
		return nil, fmt.Errorf("config key %q expects a finite number that is not negative, got %v", dotPath, value)
	case reflect.Bool:
		switch v := value.(type) {
		case bool:
			return v, nil
		case int:
			switch v {
			case 0:
				return false, nil
			case 1:
				return true, nil
			}
		case int64:
			switch v {
			case 0:
				return false, nil
			case 1:
				return true, nil
			}
		case string:
			s := strings.TrimSpace(v)
			if b, convErr := strconv.ParseBool(s); convErr == nil {
				return b, nil
			}
			switch s {
			case "0":
				return false, nil
			case "1":
				return true, nil
			}
		}
		return nil, fmt.Errorf("config key %q expects true or false, got %v", dotPath, value)
	}
	// Structs, maps and slices are sections, not values: a string from the MCP
	// tool or the CLI can never fill them, and writing it would make the file
	// undecodable.
	return nil, fmt.Errorf("config key %q is a section, not a value", dotPath)
}

func pathLabel(parts []string, i int) string {
	if i == 0 {
		return "the config root"
	}
	return fmt.Sprintf("%q", strings.Join(parts[:i], "."))
}

// Issue is a config.yaml key the schema does not know or that sits at the wrong
// nesting level. A misplaced proxy key is the dangerous case: the loader
// ignores it, so nothing breaks and nothing works.
type Issue struct {
	Key     string // dot-path as it appears in the file
	Kind    string // IssueUnknown or IssueMisplaced
	Suggest string // for a misplaced key: where it belongs
}

const (
	IssueUnknown   = "unknown"
	IssueMisplaced = "misplaced"
)

func (i Issue) String() string {
	if i.Kind == IssueMisplaced {
		return fmt.Sprintf("misplaced key %q belongs under %q, the loader ignores it here", i.Key, i.Suggest)
	}
	return fmt.Sprintf("unknown key %q, nothing reads it", i.Key)
}

// CheckKeys reports the keys of config.yaml that the schema does not know or
// that sit at the wrong nesting level. A missing file has no issues.
func CheckKeys(path string) ([]Issue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return checkKeys(raw), nil
}

// checkKeys walks a decoded document against the Config schema. Children of an
// unknown section are not reported one by one: the section itself is the
// finding.
func checkKeys(raw map[string]any) []Issue {
	var issues []Issue
	for key, value := range raw {
		fieldType, ok := yamlField(configType, key)
		if !ok {
			if _, isProxyKey := yamlField(proxyType, key); isProxyKey {
				issues = append(issues, Issue{Key: key, Kind: IssueMisplaced, Suggest: "proxy." + key})
				continue
			}
			issues = append(issues, Issue{Key: key, Kind: IssueUnknown})
			continue
		}
		collectKeyIssues(fieldType, value, key, &issues)
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Key < issues[j].Key })
	return issues
}

// collectKeyIssues walks one schema level against the decoded document value.
func collectKeyIssues(t reflect.Type, value any, path string, issues *[]Issue) {
	t = derefType(t)
	switch t.Kind() {
	case reflect.Struct:
		m, ok := value.(map[string]any)
		if !ok {
			return
		}
		for key, child := range m {
			fieldType, ok := yamlField(t, key)
			if !ok {
				*issues = append(*issues, Issue{Key: path + "." + key, Kind: IssueUnknown})
				continue
			}
			collectKeyIssues(fieldType, child, path+"."+key, issues)
		}
	case reflect.Map:
		m, ok := value.(map[string]any)
		if !ok {
			return
		}
		for key, child := range m {
			collectKeyIssues(t.Elem(), child, path+"."+key, issues)
		}
	}
}
