package proxy

import "testing"

// jsonTopLevelValueSpan ist die Grundlage des strukturellen Ersatzes im
// Fork-Builder. Die Tabelle belegt die Faelle, die der eine Integrationstest
// nicht abdeckt (YM-262).
func TestJSONTopLevelValueSpan(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		key  string
		want string
		ok   bool
	}{
		{"object value", `{"a":{"x":1},"messages":[1,2],"z":3}`, "messages", "[1,2]", true},
		{"nested object value", `{"messages":{"a":[1,{"b":2}]},"z":3}`, "messages", `{"a":[1,{"b":2}]}`, true},
		{"whitespace around value", "{\"messages\" : \n [1] , \"z\":3}", "messages", "[1]", true},
		{"string value keeps quotes", `{"model":"gpt","z":1}`, "model", `"gpt"`, true},
		{"jsonc comment before colon", `{"messages" /* c */ : [1]}`, "messages", "[1]", true},
		{"missing key", `{"a":1}`, "messages", "", false},
		{"nested key is not top level", `{"outer":{"messages":[1]}}`, "messages", "", false},
		{"top level array document", `[1,2]`, "messages", "", false},
		{"empty object", `{}`, "messages", "", false},
	}
	for _, c := range cases {
		start, end, ok := jsonTopLevelValueSpan([]byte(c.doc), c.key)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && c.doc[start:end] != c.want {
			t.Errorf("%s: span = %q, want %q", c.name, c.doc[start:end], c.want)
		}
	}
}
