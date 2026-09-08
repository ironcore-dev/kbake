// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package kernelfile parses a Kernelfile (YAML or JSON). A Kernelfile is a
// Dockerfile+kustomization-like description of how to configure and build a
// Linux kernel. See DESIGN.md for the full grammar.
//
// Parsing follows the same strategy Kubernetes uses for its manifests (see
// k8s.io/apimachinery/pkg/util/yaml): a document is JSON if its first
// non-whitespace byte is '{' (IsJSONBuffer); JSON is parsed directly and YAML
// is first converted to JSON, so every document ultimately goes through
// encoding/json and the typed structs' json tags / UnmarshalJSON methods.
package kernelfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ironcore-dev/kbake/internal/yamlorjson"

	"oras.land/oras-go/v2/registry"
)

// Options is the convenience shorthand for kernel options.
type Options struct {
	Enable  []string `json:"enable"`
	Disable []string `json:"disable"`
}

// Modules is the convenience shorthand for kernel modules.
type Modules struct {
	Enable  []string `json:"enable"`
	Disable []string `json:"disable"`
	Builtin []string `json:"builtin"`
}

// File is the convenience shorthand for a managed file.
type File struct {
	Add    *AddFile    `json:"add,omitempty"`
	Delete *DeleteFile `json:"delete,omitempty"`
}

type Files []File

type AddFile struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

type DeleteFile struct {
	Path string `json:"path"`
}

type SetConfig struct {
	Name  string      `json:"name"`
	Value ConfigValue `json:"value"`
}

type ConfigValue string

const (
	ConfigValueYes    = "Yes"
	ConfigValueNo     = "No"
	ConfigValueModule = "Module"
)

type DeleteConfig struct {
	Name string `json:"name"`
}

type Action struct {
	If           string        `json:"if,omitempty"`
	AddFile      *AddFile      `json:"addFile"`
	DeleteFile   *DeleteFile   `json:"deleteFile"`
	SetConfig    *SetConfig    `json:"setConfig"`
	DeleteConfig *DeleteConfig `json:"deleteConfig"`
}

type Actions struct {
	If      string
	Actions []Action
}

func (a *Actions) UnmarshalJSON(data []byte) error {
	obj := &struct {
		If      string   `json:"if,omitempty"`
		Actions []Action `json:"actions"`
	}{}
	if err := json.Unmarshal(data, obj); err == nil {
		*a = Actions{
			If:      obj.If,
			Actions: obj.Actions,
		}
		return nil
	}

	var actions []Action
	if err := json.Unmarshal(data, &actions); err == nil {
		*a = Actions{
			Actions: actions,
		}
		return nil
	}

	var action Action
	if err := json.Unmarshal(data, &action); err == nil {
		*a = Actions{
			Actions: []Action{action},
		}
		return nil
	}

	return fmt.Errorf("not an actions struct / action list / action struct")
}

type From string

func (f From) IsScratch() bool {
	return f == "scratch"
}

func (f From) IsGit() bool {
	return strings.HasPrefix(string(f), "git://")
}

func (f From) Git() string {
	if !f.IsGit() {
		return ""
	}
	return strings.TrimPrefix(string(f), "git://")
}

func (f From) IsRef() bool {
	return f != "" && !f.IsScratch() && !strings.Contains(string(f), "://")
}

func (f From) Ref() string {
	if !f.IsRef() {
		return ""
	}
	return string(f)
}

// Kernelfile is the parsed in-memory representation of a Kernelfile.
type Kernelfile struct {
	From    From      `json:"from"`
	Actions []Actions `json:"actions"`
	Options Options   `json:"options"`
	Modules Modules   `json:"modules"`
	Files   Files     `json:"files"`

	// file is the absolute path the Kernelfile was loaded from; used to
	// resolve relative resource references.
	file string
	// dir is the directory containing the Kernelfile (the build context root
	// for resource file resolution).
	dir string
}

// Dir returns the directory the Kernelfile lives in (build context root for
// resource file resolution).
func (k *Kernelfile) Dir() string { return k.dir }

// File returns the absolute path of the Kernelfile.
func (k *Kernelfile) File() string { return k.file }

// Load reads and parses a Kernelfile from path. path may be "-" to read from
// stdin.
func Load(path string) (*Kernelfile, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = os.ReadFile("/dev/stdin")
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read kernelfile (%s): %w", path, err)
	}
	return Parse(data, path)
}

// Parse decodes a Kernelfile from data. path is used to populate location info
// and to resolve relative references; pass "" if unknown.
//
// The document may be YAML or JSON; both are normalized to JSON (see
// kbake/internal/yaml) and then decoded with encoding/json, so the typed
// structs and their UnmarshalJSON methods drive the parsing.
func Parse(data []byte, path string) (*Kernelfile, error) {
	var k Kernelfile
	if err := yamlorjson.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("parse kernelfile: %w", err)
	}
	k.file = path
	if path != "" && path != "-" {
		abs, err := filepath.Abs(path)
		if err == nil {
			k.file = abs
			k.dir = filepath.Dir(abs)
		}
	}
	if err := Validate(&k); err != nil {
		return nil, err
	}
	return &k, nil
}

func Validate(k *Kernelfile) error {
	from := k.From
	switch {
	case from.IsGit():
		git := from.Git()
		if !strings.Contains(git, "@") {
			return errors.New("from: git address must include @<ref>")
		}
	case from.IsRef():
		ref := from.Ref()
		_, err := registry.ParseReference(ref)
		if err != nil {
			return fmt.Errorf("from: invalid reference: %w", err)
		}
	case from.IsScratch():
	default:
		return fmt.Errorf("from: invalid %q", from)
	}

	// Basic sanity for options/modules: no symbol repeated within a list.
	for _, list := range [][]string{k.Options.Enable, k.Options.Disable, k.Modules.Enable, k.Modules.Disable, k.Modules.Builtin} {
		if err := noDuplicates(list); err != nil {
			return err
		}
	}
	return nil
}

func noDuplicates(list []string) error {
	seen := map[string]bool{}
	for _, v := range list {
		if v == "" {
			return errors.New("empty symbol in options/modules list")
		}
		if seen[v] {
			return fmt.Errorf("duplicate symbol %q", v)
		}
		seen[v] = true
	}
	return nil
}
