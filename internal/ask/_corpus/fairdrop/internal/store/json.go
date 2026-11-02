//go:build !(js && wasm)

package store

import "encoding/json"

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func jsonUnmarshal(s string, v any) { _ = json.Unmarshal([]byte(s), v) }
