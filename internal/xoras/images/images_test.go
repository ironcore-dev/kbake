// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ironcore-dev/kbake/internal/xoras/config"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

type emptyJSONConfigDecoder struct{}

func (emptyJSONConfigDecoder) RecognizesDescriptor(desc ocispec.Descriptor) (bool, bool, error) {
	return desc.MediaType == ocispec.MediaTypeEmptyJSON, false, nil
}

func (emptyJSONConfigDecoder) Decode(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (config.Config, error) {
	if desc.MediaType != ocispec.MediaTypeEmptyJSON {
		return nil, fmt.Errorf("%w: not an empty JSON (%s)", errdef.ErrUnsupported, desc.MediaType)
	}
	return emptyJSONConfig{}, nil
}

type emptyJSONConfig struct{}

func (emptyJSONConfig) GetPlatform() *ocispec.Platform {
	return nil
}

var TestConfigDecoder = config.NewDecoder(
	emptyJSONConfigDecoder{},
	config.ImageDecoder,
)

type Node struct {
	Tags         []string
	MediaType    string
	ArtifactType string
	Platform     *ocispec.Platform
	Descriptor   ocispec.Descriptor
	Manifest     []byte
	Children     []Node
	Dangling     bool
}

func (n *Node) WithDangling() *Node {
	res := *n
	res.Dangling = true
	return &res
}

type NodeConfig struct {
	Tags         []string
	ArtifactType string
	Platform     *ocispec.Platform
	Children     []Node
	// Dangling makes it so if this node is a child, it's not applied.
	Dangling bool
}

func NewNodeP(t *testing.T, p *ocispec.Descriptor, mediaType string, cfg NodeConfig) *Node {
	t.Helper()
	n := NewNode(t, mediaType, cfg)
	*p = n.Descriptor
	return n
}

func NewNode(t *testing.T, mediaType string, cfg NodeConfig) *Node {
	var data []byte
	switch mediaType {
	case ocispec.MediaTypeImageIndex:
		manifests := make([]ocispec.Descriptor, 0, len(cfg.Children))
		for _, child := range cfg.Children {
			// Copy descriptor to set artifact type & platform
			childDesc := child.Descriptor
			childDesc.ArtifactType = child.ArtifactType
			childDesc.Platform = child.Platform
			manifests = append(manifests, childDesc)
		}

		index := ocispec.Index{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageIndex,
			Manifests: manifests,
		}

		var err error
		data, err = json.Marshal(index)
		if err != nil {
			t.Fatal(err)
		}
	case ocispec.MediaTypeImageManifest:
		c := ocispec.DescriptorEmptyJSON
		if cfg.Platform != nil {
			cfgData, err := json.Marshal(*cfg.Platform)
			if err != nil {
				t.Fatal(err)
			}

			c = content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, cfgData)
		}

		manifest := ocispec.Manifest{
			Versioned:    specs.Versioned{SchemaVersion: 2},
			MediaType:    ocispec.MediaTypeImageManifest,
			ArtifactType: cfg.ArtifactType,
			Config:       c,
		}

		var err error
		data, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported node media type %q", mediaType)
		return nil
	}

	desc := content.NewDescriptorFromBytes(mediaType, data)
	return &Node{
		Tags:         cfg.Tags,
		MediaType:    mediaType,
		ArtifactType: cfg.ArtifactType,
		Platform:     cfg.Platform,
		Descriptor:   desc,
		Manifest:     data,
		Children:     cfg.Children,
		Dangling:     cfg.Dangling,
	}
}

func ApplyTree(t *testing.T, target oras.Target, node *Node) {
	t.Helper()
	ctx := t.Context()

	if err := target.Push(ctx, node.Descriptor, bytes.NewReader(node.Manifest)); err != nil {
		t.Fatal(err)
	}
	for _, tag := range node.Tags {
		if err := target.Tag(ctx, node.Descriptor, tag); err != nil {
			t.Fatal(err)
		}
	}

	if node.MediaType == ocispec.MediaTypeImageManifest {
		cfgDesc := ocispec.DescriptorEmptyJSON
		cfgData := cfgDesc.Data
		if node.Platform != nil {
			var err error
			cfgData, err = json.Marshal(*node.Platform)
			if err != nil {
				t.Fatal(err)
			}

			cfgDesc = content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, cfgData)
		}

		if err := target.Push(ctx, cfgDesc, bytes.NewReader(cfgData)); err != nil {
			t.Fatal(err)
		}
	}

	for _, child := range node.Children {
		if child.Dangling {
			continue
		}
		ApplyTree(t, target, &child)
	}
}

type NodeReport struct {
	Node     *Node
	Errors   []error
	Children []*NodeReport
}

func (r *NodeReport) AnyError() bool {
	if len(r.Errors) > 0 {
		return true
	}
	for _, child := range r.Children {
		if child.AnyError() {
			return true
		}
	}
	return false
}

func (r *NodeReport) String() string {
	var sb strings.Builder
	r.formatInner(&sb, "")
	return sb.String()
}

func shortPath(descs []ocispec.Descriptor) string {
	var sb strings.Builder
	sb.WriteString("[")
	for i, desc := range descs {
		if i > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(shortDescriptor(desc))
	}
	sb.WriteString("]")
	return sb.String()
}

func equalPaths(a, b []ocispec.Descriptor) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !content.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func shortDescriptor(desc ocispec.Descriptor) string {
	if desc.MediaType == "" && desc.Digest == "" {
		return "<empty descriptor>"
	}

	_, typ, ok := strings.Cut(desc.MediaType, "/")
	if !ok {
		typ = desc.MediaType
	}

	shortDigest := desc.Digest.Encoded()[:8]
	return fmt.Sprintf("%s(%s)", typ, shortDigest)
}

func (r *NodeReport) formatInner(sb *strings.Builder, linePrefix string) {
	_, _ = fmt.Fprintf(sb, "%sNode %s\n", linePrefix, shortDescriptor(r.Node.Descriptor))
	if len(r.Errors) > 0 {
		_, _ = fmt.Fprintf(sb, "%sErrors:\n", linePrefix)
		for _, err := range r.Errors {
			_, _ = fmt.Fprintf(sb, "%s\t%s\n", linePrefix, err)
		}
	}
	if len(r.Children) > 0 {
		_, _ = fmt.Fprintf(sb, "%sChildren:\n", linePrefix)
		for _, child := range r.Children {
			child.formatInner(sb, linePrefix+"\t")
		}
	}
}

func ReportOf(t *testing.T, target oras.ReadOnlyTarget, node *Node) *NodeReport {
	t.Helper()
	ctx := t.Context()

	var reportInner func(node *Node, danglingSubtree bool) *NodeReport
	reportInner = func(node *Node, danglingSubtree bool) *NodeReport {
		rep := &NodeReport{Node: node}
		ok, err := target.Exists(ctx, node.Descriptor)
		if err != nil {
			t.Fatalf("checking if node %s exists: %v", shortDescriptor(node.Descriptor), err)
		}
		if danglingSubtree {
			if ok {
				rep.Errors = append(rep.Errors, fmt.Errorf("dangling node exists"))
			}
		} else {
			if !ok {
				rep.Errors = append(rep.Errors, fmt.Errorf("node does not exist"))
			} else {
				for _, tag := range node.Tags {
					desc, err := target.Resolve(ctx, tag)
					if err != nil {
						if !errors.Is(err, errdef.ErrNotFound) {
							t.Fatalf("resolving tag %q: %v", tag, err)
						}
						rep.Errors = append(rep.Errors, fmt.Errorf("tag %q does not exist", tag))
					} else {
						if !content.Equal(desc, node.Descriptor) {
							rep.Errors = append(rep.Errors, fmt.Errorf("tag %q has descriptor %s", tag, shortDescriptor(node.Descriptor)))
						}
					}

				}
			}
		}
		for _, child := range node.Children {
			rep.Children = append(rep.Children, reportInner(&child, danglingSubtree || child.Dangling))
		}
		return rep
	}
	return reportInner(node, false)
}

func AssertTree(t *testing.T, target oras.ReadOnlyTarget, node *Node) {
	t.Helper()
	rep := ReportOf(t, target, node)
	if rep.AnyError() {
		t.Fatalf("Tree assertion failed:\n%s", rep)
	}
}

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		name      string
		newTree   func(t *testing.T, root, expected *ocispec.Descriptor, expectedPath *[]ocispec.Descriptor) *Node
		opts      ResolveOptions
		expectErr error
	}{
		{
			name: "direct",
			newTree: func(t *testing.T, root, expected *ocispec.Descriptor, expectedPath *[]ocispec.Descriptor) *Node {
				node := NewNodeP(t, root, ocispec.MediaTypeImageManifest, NodeConfig{})
				*expected = *root
				*expectedPath = []ocispec.Descriptor{*root}
				return node
			},
		},
		{
			name: "simple index",
			newTree: func(t *testing.T, root, expected *ocispec.Descriptor, expectedPath *[]ocispec.Descriptor) *Node {
				node := NewNodeP(t, root, ocispec.MediaTypeImageIndex, NodeConfig{
					Children: []Node{
						*NewNodeP(t, expected, ocispec.MediaTypeImageManifest, NodeConfig{}),
					},
				})
				*expectedPath = []ocispec.Descriptor{*root, *expected}
				return node
			},
		},
		{
			name: "nested index",
			newTree: func(t *testing.T, root, expected *ocispec.Descriptor, expectedPath *[]ocispec.Descriptor) *Node {
				var intermediate ocispec.Descriptor
				node := NewNodeP(t, root, ocispec.MediaTypeImageIndex, NodeConfig{
					Children: []Node{
						*NewNodeP(t, &intermediate, ocispec.MediaTypeImageIndex, NodeConfig{
							Children: []Node{
								*NewNodeP(t, expected, ocispec.MediaTypeImageManifest, NodeConfig{}),
							},
						}),
					},
				})
				*expectedPath = []ocispec.Descriptor{*root, intermediate, *expected}
				return node
			},
		},
		{
			name:      "direct not found",
			expectErr: errdef.ErrNotFound,
			opts: ResolveOptions{
				MatchPlatform: MatchPlatform(nil),
			},
			newTree: func(t *testing.T, root, expected *ocispec.Descriptor, expectedPath *[]ocispec.Descriptor) *Node {
				return NewNodeP(t, root, ocispec.MediaTypeImageManifest, NodeConfig{
					Platform: &ocispec.Platform{
						OS:           "linux",
						Architecture: "amd64",
					},
				})
			},
		},
		{
			name:      "nested index sparse not found",
			expectErr: errdef.ErrNotFound,
			newTree: func(t *testing.T, root, expected *ocispec.Descriptor, expectedPath *[]ocispec.Descriptor) *Node {
				node := NewNodeP(t, root, ocispec.MediaTypeImageIndex, NodeConfig{
					Children: []Node{
						*NewNodeP(t, expected, ocispec.MediaTypeImageIndex, NodeConfig{
							Dangling: true,
						}),
					},
				})
				return node
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			local := memory.New()

			var (
				root, expected ocispec.Descriptor
				expectedPath   []ocispec.Descriptor
				tree           = tc.newTree(t, &root, &expected, &expectedPath)
			)

			ApplyTree(t, local, tree)
			opts := tc.opts
			if opts.ConfigDecoder == nil {
				opts.ConfigDecoder = TestConfigDecoder
			}
			path, err := ResolveGraph(ctx, local, root, opts)
			if tc.expectErr != nil {
				if !errors.Is(err, tc.expectErr) {
					t.Fatalf("expected error %v, got %v", tc.expectErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("got unexpected error: %v", err)
				}
				if !equalPaths(path, expectedPath) {
					t.Fatalf("expected %s, got %s", shortPath(expectedPath), shortPath(path))
				}
			}
		})
	}
}

func TestPull(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ref   string
		opts  PullOptions
		setup func(t *testing.T) (initialLocal, remote, finalLocal *Node, expectedPath []ocispec.Descriptor)

		expectErr error
	}{
		{
			name: "direct",
			ref:  "foo",
			setup: func(t *testing.T) (initialLocal, remote, finalLocal *Node, expectedPath []ocispec.Descriptor) {
				var imgDesc ocispec.Descriptor
				remote = NewNodeP(t, &imgDesc, ocispec.MediaTypeImageManifest, NodeConfig{
					Tags: []string{"foo"},
				})
				finalLocal = remote
				expectedPath = []ocispec.Descriptor{imgDesc}
				return
			},
		},
		{
			name: "simple index",
			ref:  "foo",
			setup: func(t *testing.T) (initialLocal, remote, finalLocal *Node, expectedPath []ocispec.Descriptor) {
				var (
					indexDesc ocispec.Descriptor
					imgDesc   ocispec.Descriptor
				)
				remote = NewNodeP(t, &indexDesc, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNodeP(t, &imgDesc, ocispec.MediaTypeImageManifest, NodeConfig{}),
					},
				})
				finalLocal = remote
				expectedPath = []ocispec.Descriptor{indexDesc, imgDesc}
				return
			},
		},
		{
			name: "nested index",
			ref:  "foo",
			setup: func(t *testing.T) (initialLocal, remote, finalLocal *Node, expectedPath []ocispec.Descriptor) {
				var (
					indexL1Desc, indexL2Desc, imgDesc ocispec.Descriptor
				)
				remote = NewNodeP(t, &indexL1Desc, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNodeP(t, &indexL2Desc, ocispec.MediaTypeImageIndex, NodeConfig{
							Children: []Node{
								*NewNodeP(t, &imgDesc, ocispec.MediaTypeImageManifest, NodeConfig{}),
							},
						}),
					},
				})
				finalLocal = remote
				expectedPath = []ocispec.Descriptor{indexL1Desc, indexL2Desc, imgDesc}
				return
			},
		},
		{
			name: "simple index partial existing",
			ref:  "foo",
			setup: func(t *testing.T) (initialLocal, remote, finalLocal *Node, expectedPath []ocispec.Descriptor) {
				var (
					indexDesc, imgDesc ocispec.Descriptor
				)
				initialLocal = NewNode(t, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{
							Dangling: true,
						}),
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{
							Dangling: true,
							Platform: &ocispec.Platform{
								OS:           "linux",
								Architecture: "amd64",
							},
						}),
					},
				})
				remote = NewNodeP(t, &indexDesc, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNodeP(t, &imgDesc, ocispec.MediaTypeImageManifest, NodeConfig{}),
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{
							Platform: &ocispec.Platform{
								OS:           "linux",
								Architecture: "amd64",
							},
						}),
					},
				})
				finalLocal = NewNode(t, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{}),
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{
							Dangling: true,
							Platform: &ocispec.Platform{
								OS:           "linux",
								Architecture: "amd64",
							},
						}),
					},
				})
				expectedPath = []ocispec.Descriptor{indexDesc, imgDesc}
				return //nolint:nakedret
			},
		},
		{
			name: "nested index partial existing",
			ref:  "foo",
			setup: func(t *testing.T) (initialLocal, remote, finalLocal *Node, expectedPath []ocispec.Descriptor) {
				var indexL1Desc, indexL2Desc, imgDesc ocispec.Descriptor
				initialLocal = NewNode(t, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNode(t, ocispec.MediaTypeImageIndex, NodeConfig{
							Dangling: true,
							Children: []Node{
								*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{}),
							},
						}),
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{
							Dangling: true,
							Platform: &ocispec.Platform{
								OS:           "linux",
								Architecture: "amd64",
							},
						}),
					},
				})
				remote = NewNodeP(t, &indexL1Desc, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNodeP(t, &indexL2Desc, ocispec.MediaTypeImageIndex, NodeConfig{
							Children: []Node{
								*NewNodeP(t, &imgDesc, ocispec.MediaTypeImageManifest, NodeConfig{}),
							},
						}),
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{
							Platform: &ocispec.Platform{
								OS:           "linux",
								Architecture: "amd64",
							},
						}),
					},
				})
				finalLocal = NewNode(t, ocispec.MediaTypeImageIndex, NodeConfig{
					Tags: []string{"foo"},
					Children: []Node{
						*NewNode(t, ocispec.MediaTypeImageIndex, NodeConfig{
							Children: []Node{
								*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{}),
							},
						}),
						*NewNode(t, ocispec.MediaTypeImageManifest, NodeConfig{
							Dangling: true,
							Platform: &ocispec.Platform{
								OS:           "linux",
								Architecture: "amd64",
							},
						}),
					},
				})
				expectedPath = []ocispec.Descriptor{indexL1Desc, indexL2Desc, imgDesc}
				return //nolint:nakedret
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			local := memory.New()
			remote := memory.New()

			initialLocalTree, remoteTree, finalLocalTree, expectedPath := tc.setup(t)
			if initialLocalTree != nil {
				ApplyTree(t, local, initialLocalTree)
			}
			if remoteTree != nil {
				ApplyTree(t, remote, remoteTree)
			}

			opts := tc.opts
			opts.ConfigDecoder = TestConfigDecoder
			path, err := Pull(ctx, remote, local, tc.ref, opts)
			if tc.expectErr != nil {
				if !errors.Is(err, tc.expectErr) {
					t.Fatalf("expected error %v, got %v", tc.expectErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("got unexpected error: %v", err)
				}
				if !equalPaths(expectedPath, path) {
					t.Fatalf("expected %v, got %v", expectedPath, path)
				}
				AssertTree(t, local, finalLocalTree)
			}
		})
	}
}
