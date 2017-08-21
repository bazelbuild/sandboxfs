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
	"io/ioutil"
	"os"
	"testing"

	"bazil.org/fuse"
	"golang.org/x/net/context"
)

func nodeSetup(t *testing.T) string {
	src, err := ioutil.TempDir("", "test_src_")
	if err != nil {
		t.Fatal("Setup failed with error: ", err)
	}
	return src
}

func nodeTeardown(src string, t *testing.T) {
	if err := os.RemoveAll(src); err != nil {
		t.Fatal("Teardown failed with error: ", err)
	}
}

func TestBaseNode_Inode(t *testing.T) {
	k := newBaseNode("/", DevInoPair{})
	firstInode := k.inode
	if k.inode <= 0 {
		t.Errorf("Assigned Inode number to new directory: %v, want %v", "<=0", ">0")
	}
	k = newBaseNode("/", DevInoPair{})
	if k.inode <= firstInode {
		t.Errorf("Second assigned inode number didn't increase: %v <= %v, want >", k.inode, firstInode)
	}
}

func TestBaseNode_Attr(t *testing.T) {
	src := nodeSetup(t)
	defer nodeTeardown(src, t)

	contents := "1234567890"
	if err := ioutil.WriteFile(src+"/a", []byte(contents), 0644); err != nil {
		t.Fatal("File write failed with error: ", err)
	}
	if err := os.MkdirAll(src+"/B/C", 0755); err != nil {
		t.Fatal("Mkdir failed with error: ", err)
	}

	node := newBaseNode(src+"/B", DevInoPair{})
	var a fuse.Attr
	if err := node.Attr(context.Background(), &a); err != nil {
		t.Error("Attr failed with error: ", err)
	}
	if a.Inode <= 0 {
		t.Error("Inode number is less than/equal to 0, want positive")
	}
	if got := a.Mode & os.ModePerm; got != 0755 {
		t.Errorf("Directory 'B' has incorrect permissions: %#o, want %#o", got, 0755)
	}

	node = newBaseNode(src+"/a", DevInoPair{})
	if err := node.Attr(context.Background(), &a); err != nil {
		t.Error("Attr failed with error: ", err)
	}
	if a.Inode <= 0 {
		t.Error("Inode number is less than/equal to 0, want positive")
	}
	if got := a.Mode & os.ModePerm; got != 0644 {
		t.Errorf("File 'a' has incorrect permissions: %#o, want %#o", got, 0644)
	}
	if a.Size != uint64(len(contents)) {
		t.Errorf("File 'a' has incorrect size: %d, want %d", a.Size, 10)
	}
}
