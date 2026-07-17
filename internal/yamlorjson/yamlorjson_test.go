// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package yamlorjson

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsJSONBuffer(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{"  \n\t {\"a\":1}", true}, // leading whitespace
		{"", false},
		{"   ", false},
		{"a: 1", false},
		{"# comment\na: 1", false},
		{"[1,2,3]", false}, // JSON array is not detected (only '{' is); that's the k8s heuristic
	}
	for _, c := range cases {
		if got := IsJSONBuffer([]byte(c.in)); got != c.want {
			t.Errorf("IsJSONBuffer(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestToJSONPassesJSONThrough(t *testing.T) {
	in := []byte(`{"a":1,"b":"x"}`)
	out, err := ToJSONIfYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	// JSON input is returned verbatim (so error messages stay JSON-specific).
	if string(out) != string(in) {
		t.Errorf("ToJSON modified JSON input: got %q, want %q", out, in)
	}
}

func TestToJSONConvertsYAML(t *testing.T) {
	yamldata := []byte("a: 1\nb: \"x\"\nc:\n  - 1\n  - 2\n")
	out, err := ToJSONIfYAML(yamldata)
	if err != nil {
		t.Fatal(err)
	}
	// The result must be valid JSON with the expected shape.
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("ToJSON produced invalid JSON: %v\n%s", err, out)
	}
	if v["a"].(float64) != 1 || v["b"].(string) != "x" {
		t.Errorf("unexpected JSON: %s", out)
	}
}

// Unmarshal drives decoding via encoding/json after normalizing the document.
// This is the contract the rest of kbake relies on: a custom UnmarshalJSON on
// the target is invoked for both YAML and JSON input, with no separate YAML
// path.
func TestUnmarshalInvokesUnmarshalJSONForYAML(t *testing.T) {
	type target struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	// A YAML document whose keys map to json tags.
	yamldata := []byte("kind: Option\nname: BPF_SYSCALL\n")
	var got target
	if err := Unmarshal(yamldata, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "Option" || got.Name != "BPF_SYSCALL" {
		t.Errorf("got %+v", got)
	}
}

func TestUnmarshalJSONInput(t *testing.T) {
	type target struct {
		Kind string `json:"kind"`
	}
	var got target
	if err := Unmarshal([]byte(`{"kind":"Module"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "Module" {
		t.Errorf("got %+v", got)
	}
}

func TestMarshalInvokesMarshalJSON(t *testing.T) {
	// A typed value with struct tags is rendered as YAML via the JSON path.
	type target struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	out, err := Marshal(target{Kind: "Option", Name: "BPF_SYSCALL"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: Option") || !strings.Contains(s, "name: BPF_SYSCALL") {
		t.Errorf("unexpected YAML: %s", s)
	}
}

func TestMarshalRoundtrip(t *testing.T) {
	// Marshal then Unmarshal must reproduce the value, exercising the JSON path
	// in both directions.
	type target struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		State string `json:"state"`
	}
	in := target{Kind: "Option", Name: "SECCOMP", State: "Enabled"}
	data, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out target
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", out, in)
	}
}

func TestUnmarshalErrorIsJSONSpecificForJSONInput(t *testing.T) {
	// Malformed JSON: the error should come from encoding/json, not yaml.
	err := Unmarshal([]byte(`{"kind":`), &struct{}{})
	if err == nil {
		t.Fatal("expected error")
	}
	// A JSON error mentions "invalid character" or similar; a YAML error
	// would mention "yaml:". Just assert it's not a yaml-prefixed error.
	if strings.Contains(err.Error(), "yaml:") {
		t.Errorf("expected JSON error for JSON input, got yaml error: %v", err)
	}
}
