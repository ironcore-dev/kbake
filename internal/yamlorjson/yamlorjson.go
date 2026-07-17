// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package yamlorjson converts between YAML and JSON and unmarshals documents that
// may be either. It mirrors k8s.io/apimachinery/pkg/util/yaml: a document is
// treated as JSON when its first non-whitespace byte is '{' (parsed directly)
// and as YAML otherwise (first converted to JSON). Every document ultimately
// goes through encoding/json, so a typed target (and any UnmarshalJSON method
// it implements) fully controls the decoding — there is never a separate YAML
// code path to keep in sync.
package yamlorjson

import (
	"bytes"
	"encoding/json"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ToJSONIfYAML converts a single YAML or JSON document to JSON. If the document
// appears to be JSON (its first non-whitespace byte is '{') the YAML decoding
// path is skipped, so error messages are JSON-specific. This mirrors
// k8s.io/apimachinery/pkg/util/yamlorjson.ToJSON.
func ToJSONIfYAML(data []byte) ([]byte, error) {
	if IsJSONBuffer(data) {
		return data, nil
	}
	return yamlToJSON(data)
}

// IsJSONBuffer reports whether data begins (after leading whitespace) with
// '{', the heuristic Kubernetes uses to tell a JSON document from YAML. Since
// JSON is a strict subset of YAML, a YAML document that happens to start with
// '{' would round-trip through the YAML path identically, so this is only an
// optimization (and gives JSON-specific error messages for JSON input).
func IsJSONBuffer(data []byte) bool {
	trim := bytes.TrimLeftFunc(data, unicode.IsSpace)
	return bytes.HasPrefix(trim, []byte("{"))
}

// Unmarshal unmarshals a YAML or JSON document into v. The document is first
// converted to JSON (see ToJSONIfYAML) and then handed to encoding/json, so v's
// type and any UnmarshalJSON method drive the decoding — there is no
// separate YAML unmarshal path. This mirrors the behavior of
// sigs.k8s.io/yamlorjson.Unmarshal for typed targets.
func Unmarshal(data []byte, v any) error {
	jsonData, err := ToJSONIfYAML(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonData, v)
}

// Marshal renders v as YAML. v is first marshaled to JSON with encoding/json
// (so v's type and any MarshalJSON method, plus struct tags, drive the
// output) and the JSON is then converted to YAML. This is the symmetric
// inverse of Unmarshal: there is no separate YAML marshal path to keep in
// sync. It mirrors sigs.k8s.io/yamlorjson.Marshal.
func Marshal(v any) ([]byte, error) {
	jsonData, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jsonToYAML(jsonData)
}

// yamlToJSON converts a YAML document to JSON by decoding it into a generic
// Go value and re-encoding as JSON — the same two-step approach used by
// sigs.k8s.io/yaml.YAMLToJSON (k8s uses gopkg.in/yaml.v2; this reuses the
// yaml.v3 already in the module).
func yamlToJSON(data []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// jsonToYAML converts a JSON document to YAML by decoding it into a generic
// Go value and re-encoding as YAML — the inverse of yamlToJSON and the same
// approach used by sigs.k8s.io/yaml.JSONToYAML.
func jsonToYAML(data []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return yaml.Marshal(v)
}
