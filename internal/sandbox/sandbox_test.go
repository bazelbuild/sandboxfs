// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License.  You may obtain a copy
// of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  See the
// License for the specific language governing permissions and limitations
// under the License.

package sandbox

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

// areTreesSimilar checks if two directory trees are similar and returns nil if they are, or an
// error if they are not.
//
// Two trees are similar if they contain the same set of nodes, the nodes are of the same types, and
// the underlying paths for the nodes match.
//
// The path argument represents the current location of the got and want nodes relative to the root
// of the hierarchy.  This value is used purely for error reporting purposes.
func areTreesSimilar(path string, got Node, want Node) error {
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		return fmt.Errorf("in %s: got type %v, want %v", path, reflect.TypeOf(got), reflect.TypeOf(want))
	}

	gotUnderlyingPath, gotIsMapping := got.UnderlyingPath()
	wantUnderlyingPath, wantIsMapping := want.UnderlyingPath()
	if gotIsMapping != wantIsMapping || gotUnderlyingPath != wantUnderlyingPath {
		return fmt.Errorf("in %s: got isMapping=%v underlyingPath=%v, want isMapping=%v, underlyingPath=%v", path, gotIsMapping, gotUnderlyingPath, wantIsMapping, wantUnderlyingPath)
	}

	if gotDir, ok := got.(*Dir); ok {
		wantDir := want.(*Dir)

		for gotName, gotNode := range gotDir.children {
			if wantNode, ok := wantDir.children[gotName]; ok {
				if err := areTreesSimilar(filepath.Join(path, gotName), gotNode, wantNode); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("in %s: extra entry %s", path, gotName)
			}
		}

		for wantName := range wantDir.children {
			if _, ok := gotDir.children[wantName]; !ok {
				return fmt.Errorf("in %s: missing entry %s", path, wantName)
			}
		}
	}
	return nil
}

func TestCreateRoot_Ok(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	nestedTempDir := filepath.Join(tempDir, "nested-dir")
	utils.MustMkdirAll(t, nestedTempDir, 0755)
	nestedTempFile := filepath.Join(tempDir, "file")
	utils.MustWriteFile(t, nestedTempFile, 0644, "")
	nestedTempSymlink := filepath.Join(tempDir, "symlink")
	utils.MustSymlink(t, "/non-existent", nestedTempSymlink)

	testData := []struct {
		name     string
		mappings []MappingSpec
		wantDir  *Dir
	}{
		{
			"JustRoot",
			[]MappingSpec{
				{Mapping: "/", Target: tempDir},
			},
			&Dir{
				BaseNode: BaseNode{optionalUnderlyingPath: tempDir},
			},
		},
		{
			"OneMappingInRoot",
			[]MappingSpec{
				{Mapping: "/", Target: tempDir},
				{Mapping: "/foo", Target: nestedTempFile},
			},
			&Dir{
				BaseNode: BaseNode{optionalUnderlyingPath: tempDir},
				children: map[string]Node{
					"foo": &File{
						BaseNode: BaseNode{optionalUnderlyingPath: nestedTempFile},
					},
				},
			},
		},
		{
			"OneMappingInScaffoldDir",
			[]MappingSpec{
				{Mapping: "/foo/bar/baz", Target: nestedTempFile},
			},
			&Dir{
				children: map[string]Node{
					"foo": &Dir{
						children: map[string]Node{
							"bar": &Dir{
								children: map[string]Node{
									"baz": &File{
										BaseNode: BaseNode{optionalUnderlyingPath: nestedTempFile},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			"ReusedDirectoriesDontLeakScaffoldEntries",
			[]MappingSpec{
				// If the directory nodes were shared (as used to be the case due to
				// caching based on underlying path), the /foo/dir mapping below
				// would mistakenly reexpose a "dir" entry.
				{Mapping: "/foo", Target: tempDir},
				{Mapping: "/foo/dir", Target: tempDir},
			},
			&Dir{
				children: map[string]Node{
					"foo": &Dir{
						BaseNode: BaseNode{optionalUnderlyingPath: tempDir},
						children: map[string]Node{
							"dir": &Dir{
								BaseNode: BaseNode{optionalUnderlyingPath: tempDir},
							},
						},
					},
				},
			},
		},
		{
			"MixedMappingsAndScaffoldDirs",
			[]MappingSpec{
				{Mapping: "/foo/bar", Target: tempDir},
				{Mapping: "/foo/bar/baz/dup", Target: nestedTempDir},
				{Mapping: "/foo/bar/baz/symlink", Target: nestedTempSymlink},
				{Mapping: "/foo/bar/file", Target: nestedTempFile},
			},
			&Dir{
				children: map[string]Node{
					"foo": &Dir{
						children: map[string]Node{
							"bar": &Dir{
								BaseNode: BaseNode{optionalUnderlyingPath: tempDir},
								children: map[string]Node{
									"baz": &Dir{
										children: map[string]Node{
											"dup": &Dir{
												BaseNode: BaseNode{optionalUnderlyingPath: nestedTempDir},
											},
											"symlink": &Symlink{
												BaseNode: BaseNode{optionalUnderlyingPath: nestedTempSymlink},
											},
										},
									},
									"file": &File{
										BaseNode: BaseNode{optionalUnderlyingPath: nestedTempFile},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			root, err := CreateRoot(nil, d.mappings)
			if err != nil {
				t.Fatalf("Failed to create hierarchy: %v", err)
			}
			if err := areTreesSimilar("/", root, d.wantDir); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestCreateRoot_Errors(t *testing.T) {
	testData := []struct {
		name     string
		mappings []MappingSpec
		wantErr  string
	}{
		{
			"MissingTarget",
			[]MappingSpec{
				{Mapping: "/", Target: "/non-existent"},
			},
			`failed to stat /non-existent when mapping /`,
		},
		{
			"EmptyMappingPath",
			[]MappingSpec{
				{Mapping: "foo/../.", Target: "/"},
			},
			`/\.\./\.:.*empty path`,
		},
		{
			"RelativeMappingPath",
			[]MappingSpec{
				{Mapping: "foo/bar", Target: "/"},
			},
			`foo/bar:.must be.*absolute`,
		},
		{
			"RootMappedTwice",
			[]MappingSpec{
				{Mapping: "/", Target: "/"},
				{Mapping: "/", Target: "/"},
			},
			`failed to map root.*target /:.*not more than once`,
		},
		{
			"RootMappedTooLate",
			[]MappingSpec{
				{Mapping: "/foo", Target: "/"},
				{Mapping: "/", Target: "/tmp"},
			},
			`failed to map root.*target /tmp:.*mapped first`,
		},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			_, err := CreateRoot(nil, d.mappings)
			if err == nil {
				t.Errorf("want error to match %s; got no error", d.wantErr)
			} else {
				if !utils.MatchesRegexp(d.wantErr, err.Error()) {
					t.Errorf("want error to match %s; got %v", d.wantErr, err)
				}
			}
		})
	}
}

func TestTokenizePath(t *testing.T) {
	testData := []struct {
		name string

		path       string
		wantTokens []string
	}{
		{"Empty", "", []string{}},
		{"EmptyUnclean", "foo/../bar/./..", []string{}},

		{"Root", "/", []string{""}},

		{"AbsoluteOneComponent", "/foo", []string{"", "foo"}},
		{"AbsoluteOneComponentUnclean", "/foo/./bar/..", []string{"", "foo"}},
		{"AbsoluteManyComponents", "/foo/b/baz", []string{"", "foo", "b", "baz"}},

		{"RelativeOneComponent", "a", []string{"a"}},
		{"RelativeOneComponentUnclean", "a/../a/.", []string{"a"}},
		{"RelativeManyComponents", "a/bar/z", []string{"a", "bar", "z"}},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			tokens := tokenizePath(d.path)
			if !reflect.DeepEqual(tokens, d.wantTokens) {
				t.Errorf("got %v, want %v", tokens, d.wantTokens)
			}
		})
	}
}
