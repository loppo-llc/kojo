package server

import "testing"

func TestValidateStatusJSON(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantOK  bool
	}{
		{"flat strings", `{"mood":"good","energy":"low"}`, true},
		{"typed scalars", `{"fatigue_level": 3, "awake": true}`, true},
		{"empty object", `{}`, true},
		{"array top-level", `["a","b"]`, false},
		{"string top-level", `"hello"`, false},
		{"nested object", `{"mood":{"value":"good"}}`, false},
		{"array value", `{"tags":["a"]}`, false},
		{"null value", `{"mood":null}`, false},
		{"invalid json", `{"mood":`, false},
		{"trailing garbage", `{"a":"b"}{"c":"d"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateStatusJSON(c.content)
			if (err == nil) != c.wantOK {
				t.Fatalf("validateStatusJSON(%q) err=%v, wantOK=%v", c.content, err, c.wantOK)
			}
		})
	}
}
