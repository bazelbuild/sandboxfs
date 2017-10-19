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
	"os"
	"sort"
	"syscall"
	"testing"

	"bazil.org/fuse"
	"golang.org/x/net/context"
)

func TestScaffoldDir_Attr(t *testing.T) {
	dir := newScaffoldDir()
	a := new(fuse.Attr)
	err := dir.Attr(context.Background(), a)
	if err != nil {
		t.Errorf("Attr on scaffold directory returned error: %v, want: nil", err)
	}
	if want := 0555 | os.ModeDir; a.Mode != want {
		t.Errorf("Attr on scaffold directory returned mode: %#o, want %#o", a.Mode, want)
	}
}

func TestScaffoldDir_Lookup(t *testing.T) {
	dir := newScaffoldDir()
	node, err := dir.Lookup(context.Background(), "A")
	if err != fuseErrno(syscall.ENOENT) {
		t.Errorf("Lookup on empty node returned error: nil, want: syscall.ENOENT")
	}
	if node != nil {
		t.Errorf("Lookup on empty node returned node: %v, want: nil", node)
	}

	dir.mappedChildren["A"] = newMappedDir("/", DevInoPair{}, false)
	node, err = dir.Lookup(context.Background(), "A")
	if err != nil {
		t.Errorf("Lookup returned error: %v, want: %v", err, nil)
	}
	if _, ok := node.(*MappedDir); !ok {
		t.Errorf("Lookup returned node of type: %T, want: *MappedDir", node)
	}

	dir.scaffoldDirs["B"] = newScaffoldDir()
	node, err = dir.Lookup(context.Background(), "B")
	if err != nil {
		t.Errorf("Lookup returned error: %v, want: %v", err, nil)
	}
	if _, ok := node.(*ScaffoldDir); !ok {
		t.Errorf("Lookup returned node of type: %T, want: *ScaffoldDir", node)
	}

	dir.mappedChildren["B"] = newMappedDir("/", DevInoPair{}, false)
	node, err = dir.Lookup(context.Background(), "B")
	if err != nil {
		t.Errorf("Lookup returned error: %v, want: %v", err, nil)
	}
	if _, ok := node.(*MappedDir); !ok {
		t.Errorf("Lookup returned node of type: %T, want: *MappedDir", node)
	}
}

func TestOpenScaffoldDir_ReadDirAll(t *testing.T) {
	dir := newScaffoldDir()
	dir.mappedChildren["A"] = newMappedDir("/", DevInoPair{}, false)
	dir.mappedChildren["C"] = newMappedFile("/", DevInoPair{}, false)
	dir.scaffoldDirs["B"] = newScaffoldDir()
	dir.scaffoldDirs["C"] = newScaffoldDir()

	openDir := &OpenScaffoldDir{dir}
	nodes, err := openDir.ReadDirAll(context.Background())
	if err != nil {
		t.Errorf("Lookup returned error: %v, want: %v", err, nil)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	if l := len(nodes); l != 3 {
		t.Errorf("Length of nodelist is %v, want: %v", l, 3)
	}
	if n := nodes[0]; n.Name != "A" || n.Type != fuse.DT_Dir {
		t.Errorf("Returned element is: %v, want: {A, fuse.DT_Dir}", n)
	}
	if n := nodes[1]; n.Name != "B" || n.Type != fuse.DT_Dir {
		t.Errorf("Returned element is: %v, want: {B, fuse.DT_Dir}", n)
	}
	if n := nodes[2]; n.Name != "C" || n.Type != fuse.DT_File {
		t.Errorf("Returned element is: %v, want: {C, fuse.DT_File}", n)
	}
}

func TestScaffoldDir_ScaffoldDirChild(t *testing.T) {
	dir := newScaffoldDir()
	dir.scaffoldDirs["b"] = newScaffoldDir()
	dir.scaffoldDirs["c"] = newScaffoldDir()

	if newDir := dir.scaffoldDirChild("a"); newDir != dir.scaffoldDirs["a"] {
		t.Errorf("Child returned was not persistent")
	}
	if existing := dir.scaffoldDirs["b"]; existing != dir.scaffoldDirChild("b") {
		t.Errorf("Incorrect child was returned for already existing scaffold child.")
	}
}

func TestScaffoldDir_NewNodeChild(t *testing.T) {
	vd := newScaffoldDir()
	writable := true
	d, err := vd.newNodeChild("a", "/", writable)
	if err != nil {
		t.Errorf("newNodeChild returned error: %v, want: nil", err)
	}

	dir, ok := d.(*MappedDir)
	if !ok {
		t.Fatalf("Returned node is of type: %T, want (*MappedDir)", d)
	}
	if got := dir.underlyingPath; got != "/" {
		t.Errorf("Returned directory node has underlyingPath: %q, want: '/'", got)
	}
	if got := dir.writable; got != writable {
		t.Errorf("Returned directory node has writable: %v, want: %v", got, writable)
	}

	if _, err := vd.newNodeChild("a", "/", writable); err == nil {
		t.Errorf("Attempting to create node over existing node gave error nil, want: non-nil")
	}
}
