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
	"testing"

	"bazil.org/fuse"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

func TestVirtualDir_Attr(t *testing.T) {
	dir := newVirtualDir()
	a := new(fuse.Attr)
	err := dir.Attr(context.Background(), a)
	if err != nil {
		t.Errorf("Attr on virtual directory returned error: %v, want: nil", err)
	}
	if want := 0555 | os.ModeDir; a.Mode != want {
		t.Errorf("Attr on virtual directory returned mode: %#o, want %#o", a.Mode, want)
	}
}

func TestVirtualDir_Lookup(t *testing.T) {
	dir := newVirtualDir()
	node, err := dir.Lookup(context.Background(), "A")
	if err != fuseErrno(unix.ENOENT) {
		t.Errorf("Lookup on empty node returned error: nil, want: unix.ENOENT")
	}
	if node != nil {
		t.Errorf("Lookup on empty node returned node: %v, want: nil", node)
	}

	dir.mappedChildren["A"] = newDir("/", DevInoPair{}, false)
	node, err = dir.Lookup(context.Background(), "A")
	if err != nil {
		t.Errorf("Lookup returned error: %v, want: %v", err, nil)
	}
	if _, ok := node.(*Dir); !ok {
		t.Errorf("Lookup returned node of type: %T, want: *Dir", node)
	}

	dir.virtualDirs["B"] = newVirtualDir()
	node, err = dir.Lookup(context.Background(), "B")
	if err != nil {
		t.Errorf("Lookup returned error: %v, want: %v", err, nil)
	}
	if _, ok := node.(*VirtualDir); !ok {
		t.Errorf("Lookup returned node of type: %T, want: *Dir", node)
	}

	dir.mappedChildren["B"] = newDir("/", DevInoPair{}, false)
	node, err = dir.Lookup(context.Background(), "B")
	if err != nil {
		t.Errorf("Lookup returned error: %v, want: %v", err, nil)
	}
	if _, ok := node.(*Dir); !ok {
		t.Errorf("Lookup returned node of type: %T, want: *Dir", node)
	}
}

func TestOpenVirtualDir_ReadDirAll(t *testing.T) {
	dir := newVirtualDir()
	dir.mappedChildren["A"] = newDir("/", DevInoPair{}, false)
	dir.mappedChildren["C"] = newFile("/", DevInoPair{}, false)
	dir.virtualDirs["B"] = newVirtualDir()
	dir.virtualDirs["C"] = newVirtualDir()

	openDir := &OpenVirtualDir{dir}
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

func TestVirtualDir_VirtualDirChild(t *testing.T) {
	dir := newVirtualDir()
	dir.virtualDirs["b"] = newVirtualDir()
	dir.virtualDirs["c"] = newVirtualDir()

	if newDir := dir.virtualDirChild("a"); newDir != dir.virtualDirs["a"] {
		t.Errorf("Child returned was not persistent")
	}
	if existing := dir.virtualDirs["b"]; existing != dir.virtualDirChild("b") {
		t.Errorf("Incorrect child was returned for already existing virtual child.")
	}
}

func TestVirtualDir_NewNodeChild(t *testing.T) {
	vd := newVirtualDir()
	writable := true
	d, err := vd.newNodeChild("a", "/", writable)
	if err != nil {
		t.Errorf("newNodeChild returned error: %v, want: nil", err)
	}

	dir, ok := d.(*Dir)
	if !ok {
		t.Fatalf("Returned node is of type: %T, want (*Dir)", d)
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
