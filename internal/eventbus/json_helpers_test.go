package eventbus

import (
	"encoding/json"
	"strings"
)

// jsonMarshal / contains keep the wire-format test in bus_test.go free
// of stdlib boilerplate — they are not exported helpers.

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
