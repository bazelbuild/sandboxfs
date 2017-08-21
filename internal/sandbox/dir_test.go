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
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"

	"bazil.org/fuse"
	"golang.org/x/net/context"
)

var OpenRequestDir = &fuse.OpenRequest{
	Dir:   true,
	Flags: fuse.OpenReadOnly,
}

func dirSetup(t *testing.T) string {
	src, err := ioutil.TempDir("", "test_src_")
	if err != nil {
		t.Fatal("Setup failed with error: ", err)
	}
	return src
}

func dirTeardown(src string, t *testing.T) {
	if err := os.RemoveAll(src); err != nil {
		t.Fatal("Teardown failed with error: ", err)
	}
}

func TestDir_NewDirFromExisting(t *testing.T) {
	for _, writable := range []bool{false, true} {
		vd := newVirtualDir()
		vd.mappedChildren["a"] = newDir("/", DevInoPair{}, writable)
		vd.mappedChildren["c"] = newFile("/", DevInoPair{}, writable)
		vd.virtualDirs["b"] = newVirtualDir()
		vd.virtualDirs["c"] = newVirtualDir()

		testID := DevInoPair{2, 3}
		d := newDirFromExisting(vd.mappedChildren, vd.virtualDirs, "/", testID, writable)
		if !reflect.DeepEqual(vd.mappedChildren, d.mappedChildren) {
			t.Errorf("newDirFromExisting result had mappedChildren %v, want %v", d.mappedChildren, vd.mappedChildren)
		}
		if !reflect.DeepEqual(vd.virtualDirs, d.virtualDirs) {
			t.Errorf("newDirFromExisting result had virtualDirs %v, want %v", d.mappedChildren, vd.mappedChildren)
		}
		if d.underlyingID != testID {
			t.Errorf("Dir got underlyingID: %v, want: %v ", d.underlyingID, testID)
		}
		if d.inode == vd.inode {
			t.Errorf("Dir should not have preserved VirtualDir's inode number, but did")
		}
	}
}

func TestDir_Lookup(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	if err := os.MkdirAll(src+"/b", 0755); err != nil {
		t.Fatal("MkdirAll failed with error: ", err)
	}
	if err := ioutil.WriteFile(src+"/b/c", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}

	for _, d := range []*Dir{
		newDir(src, DevInoPair{}, false),
		newDir(src, DevInoPair{}, true),
	} {
		node, err := d.Lookup(context.Background(), "b")
		if err != nil {
			t.Error("Lookup failed with error:", err)
		}
		childB, ok := node.(*Dir)
		if !ok {
			t.Errorf("Node is of type %T, want: (*Dir)", node)
		}

		node, err = childB.Lookup(context.Background(), "c")
		if err != nil {
			t.Error("Lookup failed with error:", err)
		}
		if _, ok := node.(*File); !ok {
			t.Errorf("Node is of type %T, want: (*File)", node)
		}
	}
}

func TestDir_Lookup_PreferMapped(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	if err := os.MkdirAll(src+"/a", 0755); err != nil {
		t.Fatal("MkdirAll failed with error: ", err)
	}

	for _, writable := range []bool{false, true} {
		dir := newDir(src, DevInoPair{}, writable)
		dir.mappedChildren["a"] = newDir("/", DevInoPair{}, writable)
		dir.virtualDirs["a"] = newVirtualDir()

		node, err := dir.Lookup(context.Background(), "a")
		if err != nil {
			t.Error("Lookup failed with error:", err)
		}
		childA, ok := node.(*Dir)
		if !ok {
			t.Errorf("Node is of type %T, want: (*Dir)", node)
		}
		if childA.underlyingPath != "/" {
			t.Errorf("Node has underlyingPath %q, want: %q", childA.underlyingPath, "/")
		}
	}
}

func TestDir_BaseOverVirtual(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	if err := os.MkdirAll(src+"/b/c", 0755); err != nil {
		t.Fatal("MkdirAll failed with error: ", err)
	}

	for _, writable := range []bool{false, true} {
		dir := newDir(src, DevInoPair{}, writable)
		dir.virtualDirs["b"] = newVirtualDir()

		node, err := dir.Lookup(context.Background(), "b")
		if err != nil {
			t.Error("Lookup failed with error:", err)
		}
		childB, ok := node.(*Dir)
		if !ok {
			t.Errorf("Node is of type %T, want: (*Dir)", node)
		}
		if childB.underlyingPath != src+"/b" {
			t.Errorf("Node has underlyingPath %q, want: %q", childB.underlyingPath, src+"/b")
		}

		handle, err := dir.Open(context.Background(), OpenRequestDir, nil)
		if err != nil {
			t.Error("Directory open failed with error: ", err)
		}
		got, err := handle.(*OpenDir).ReadDirAll(context.Background())
		if err != nil {
			t.Errorf("ReadDirAll failed with error: %v", err)
		}
		want := []fuse.Dirent{childB.Dirent("b")}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("ReadDirAll mismatch: got dirents: %v, want: %v", got, want)
		}
	}
}

func TestDir_VirtualOverBaseFile(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	if err := ioutil.WriteFile(src+"/b", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}

	for _, writable := range []bool{false, true} {
		dir := newDir(src, DevInoPair{}, writable)
		dir.virtualDirs["b"] = newVirtualDir()

		node, err := dir.Lookup(context.Background(), "b")
		if err != nil {
			t.Error("Lookup failed with error:", err)
		}
		childB, ok := node.(*VirtualDir)
		if !ok {
			t.Errorf("Node is of type %T, want: (*VirtualDir)", node)
		}

		handle, err := dir.Open(context.Background(), OpenRequestDir, nil)
		if err != nil {
			t.Error("Directory open failed with error: ", err)
		}
		got, err := handle.(*OpenDir).ReadDirAll(context.Background())
		if err != nil {
			t.Errorf("ReadDirAll failed with error: %v", err)
		}
		want := []fuse.Dirent{childB.Dirent("b")}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("ReadDirAll mismatch: got dirents: %v, want: %v", got, want)
		}
	}
}

func TestDir_ReadDirAll(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	if err := os.MkdirAll(src+"/b/c", 0755); err != nil {
		t.Fatal("MkdirAll failed with error: ", err)
	}
	if err := ioutil.WriteFile(src+"/b/d", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}
	if err := ioutil.WriteFile(src+"/b/e", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}

	for _, writable := range []bool{false, true} {
		d := newDir(src, DevInoPair{}, writable)

		node, err := d.Lookup(context.Background(), "b")
		if err != nil {
			t.Error("Directory lookup failed with error: ", err)
		}
		handle, err := node.(*Dir).Open(context.Background(), OpenRequestDir, nil)
		if err != nil {
			t.Error("Directory open failed with error: ", err)
		}
		dirents, err := handle.(*OpenDir).ReadDirAll(context.Background())
		if err != nil {
			t.Errorf("ReadDirAll failed with error: %v", err)
		}

		sort.Slice(dirents, func(i, j int) bool { return dirents[i].Name < dirents[j].Name })
		inodeNums := make(map[uint64]bool)
		for i, fileInfo := range dirents {
			if l := []string{"c", "d", "e"}; fileInfo.Name != l[i] {
				t.Errorf("ReadDirAll failed. got: %q, want %q", fileInfo.Name, l[i])
			}
			if _, ok := inodeNums[fileInfo.Inode]; ok {
				t.Errorf("Got inode number %v more than once in ReadDirAll", fileInfo.Inode)
			}
			inodeNums[fileInfo.Inode] = true
		}
		if err := handle.(*OpenDir).Release(context.Background(), nil); err != nil {
			t.Error("Directory close failed with error: ", err)
		}
	}
}

func TestDir_Mkdir_ReadOnly_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, false)
	mkdirReq := fuse.MkdirRequest{Name: "newDir"}

	if _, err := d.Mkdir(context.Background(), &mkdirReq); err != fuseErrno(syscall.EPERM) {
		t.Errorf("Mkdir on a read-only directory gave error %T(%v), want fuseErrno(syscall.EPERM)", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.underlyingPath, mkdirReq.Name)); !os.IsNotExist(err) {
		t.Errorf("Stat got error: %v, want file not found", err)
	}
}

func TestDir_Mkdir_ReadWrite_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, true)
	if err := os.MkdirAll(src+"/newDir", 0755); err != nil {
		t.Fatal("MkdirAll failed with error: ", err)
	}

	req := fuse.MkdirRequest{
		Name: "newDir",
		Mode: os.ModeDir | 0755,
	}
	_, err := d.Mkdir(context.Background(), &req)
	if e, ok := err.(fuse.Errno); !ok || !os.IsExist(syscall.Errno(e)) {
		t.Errorf("Mkdir returned error: %v, want fuse.Errno(os.IsExist(err))", err)
	}
}

func TestDir_Mkdir_ReadWrite_Ok(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, true)
	const perm = 0755
	req := fuse.MkdirRequest{
		Name: "newDir",
		Mode: os.ModeDir | perm,
	}

	n, err := d.Mkdir(context.Background(), &req)
	if err != nil {
		t.Errorf("Mkdir failed with error: %v", err)
	}

	if want := filepath.Join(d.underlyingPath, req.Name); n.(Node).UnderlyingPath() != want {
		t.Errorf("Underlying directory path for created directory: %q, want %q", n.(Node).UnderlyingPath(), want)
	}

	stat, err := os.Stat(n.(Node).UnderlyingPath())
	if err != nil || !stat.IsDir() {
		t.Errorf("Expected directory in the underlying filesystem")
	}
	if got := stat.Mode() & os.ModePerm; got != perm {
		t.Errorf("Got node permissions: %v, want: %v", got, perm)
	}
}

func TestDir_Mknod_ReadOnly_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, false)
	mknodReq := fuse.MknodRequest{Name: "newNode"}

	if _, err := d.Mknod(context.Background(), &mknodReq); err != fuseErrno(syscall.EPERM) {
		t.Errorf("Mknod on a read-only directory gave error %T(%v), want fuseErrno(syscall.EPERM)", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.underlyingPath, mknodReq.Name)); !os.IsNotExist(err) {
		t.Errorf("Stat got error: %v, want file not found", err)
	}
}

func TestDir_Mknod_ReadWrite_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, true)
	if err := os.MkdirAll(src+"/newNode", 0755); err != nil {
		t.Fatal("MkdirAll failed with error: ", err)
	}

	req := fuse.MknodRequest{
		Name: "newNode",
		Mode: syscall.S_IFIFO | 0644,
	}

	_, err := d.Mknod(context.Background(), &req)
	if e, ok := err.(fuse.Errno); !ok || !os.IsExist(syscall.Errno(e)) {
		t.Errorf("Mknod returned error: %v, want fuse.Errno(os.IsExist(err))", err)
	}
}

func TestDir_Mknod_ReadWrite_Ok(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, true)
	const perm = 0644
	req := fuse.MknodRequest{
		Name: "newNode",
		Mode: syscall.S_IFIFO | perm,
	}

	n, err := d.Mknod(context.Background(), &req)
	if err != nil {
		t.Errorf("Mknod failed with error: %v", err)
	}

	if want := filepath.Join(d.underlyingPath, req.Name); n.(Node).UnderlyingPath() != want {
		t.Errorf("Underlying directory path for created node: %q, want %q", n.(Node).UnderlyingPath(), want)
	}
	stat, err := os.Stat(n.(Node).UnderlyingPath())
	if err != nil {
		t.Errorf("Underlying filesystem gave error for new node: %v, want nil", err)
	}
	if got := stat.Mode() & os.ModePerm; got != perm {
		t.Errorf("Got node permissions: %v, want: %v", got, perm)
	}
}

func TestDir_Create_ReadOnly_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, false)
	createReq := fuse.CreateRequest{Name: "newFile"}
	createResp := fuse.CreateResponse{}

	if _, _, err := d.Create(context.Background(), &createReq, &createResp); err != fuseErrno(syscall.EPERM) {
		t.Errorf("Create on a read-only directory gave error %T(%v), want fuseErrno(syscall.EPERM)", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.underlyingPath, createReq.Name)); !os.IsNotExist(err) {
		t.Errorf("Stat got error: %v, want file not found", err)
	}
}

func TestDir_Create_ReadWrite_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)
	if err := os.MkdirAll(src+"/newFile", 0755); err != nil {
		t.Fatal("MkdirAll failed with error: ", err)
	}

	d := newDir(src, DevInoPair{}, true)
	req := fuse.CreateRequest{
		Name:  "newFile",
		Flags: fuse.OpenFlags(os.O_CREATE | os.O_TRUNC),
		Mode:  0644,
	}
	resp := fuse.CreateResponse{}
	_, _, err := d.Create(context.Background(), &req, &resp)
	if _, ok := err.(fuse.Errno); !ok {
		t.Errorf("Create returned error: %T(%v), want fuse.Errno", err, err)
	}
}

func TestDir_Create_ReadWrite_Ok(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, true)
	const perm = 0755
	req := fuse.CreateRequest{
		Name:  "newFile",
		Flags: fuse.OpenFlags(os.O_CREATE | os.O_TRUNC),
		Mode:  os.ModeDir | perm,
	}
	resp := fuse.CreateResponse{}

	n, o, err := d.Create(context.Background(), &req, &resp)
	if err != nil {
		t.Errorf("Create failed with error: %v", err)
	}

	if want := filepath.Join(d.underlyingPath, req.Name); n.(Node).UnderlyingPath() != want {
		t.Errorf("Underlying directory path for created file: %q, want %q", n.(Node).UnderlyingPath(), want)
	}
	stat, err := os.Stat(n.(Node).UnderlyingPath())
	if err != nil {
		t.Errorf("Underlying filesystem gave error for new file: %v, want nil", err)
	}
	if got := stat.Mode() & os.ModePerm; got != perm {
		t.Errorf("Got node permissions: %v, want: %v", got, perm)
	}

	if o.(*OpenFile).file != n {
		t.Errorf("The handle returned from create does not point to the returned node")
	}
}

func TestDir_Symlink_ReadOnly_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, false)
	symlinkReq := fuse.SymlinkRequest{NewName: "newNode", Target: "."}

	if _, err := d.Symlink(context.Background(), &symlinkReq); err != fuseErrno(syscall.EPERM) {
		t.Errorf("Symlink on a read-only directory gave error %T(%v), want fuseErrno(syscall.EPERM)", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.underlyingPath, symlinkReq.NewName)); !os.IsNotExist(err) {
		t.Errorf("Stat got error: %v, want file not found", err)
	}
}

func TestDir_Symlink_ReadWrite_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)
	if err := os.Chmod(src, 0444); err != nil {
		t.Errorf("Chmod failed with error: %v", err)
	}

	d := newDir(src, DevInoPair{}, true)
	symlinkReq := fuse.SymlinkRequest{NewName: "newNode", Target: "."}

	if _, err := d.Symlink(context.Background(), &symlinkReq); err != fuseErrno(syscall.EACCES) {
		t.Errorf("Symlink gave error %T(%v), want fuseErrno(syscall.EACCES)", err, err)
	}
}

func TestDir_Symlink_ReadWrite_Ok(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, true)
	req := fuse.SymlinkRequest{
		NewName: "newSymlink",
		Target:  "..",
	}

	n, err := d.Symlink(context.Background(), &req)
	if err != nil {
		t.Errorf("Symlink failed with error: %v", err)
	}

	if want := filepath.Join(d.underlyingPath, req.NewName); n.(Node).UnderlyingPath() != want {
		t.Errorf("Underlying directory path for created symlink: %q, want %q", n.(Node).UnderlyingPath(), want)
	}
	if _, err := os.Stat(n.(Node).UnderlyingPath()); err != nil {
		t.Errorf("Underlying filesystem gave error for new file: %v, want nil", err)
	}
	if str, err := os.Readlink(n.(Node).UnderlyingPath()); err != nil || str != req.Target {
		t.Errorf("Symlink returned (%q, %v), want: (%q, nil)", str, err, req.Target)
	}
}

func TestDir_Rename_ReadOnly_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, true)
	renameReq := fuse.RenameRequest{OldName: "a", NewName: "b"}

	err := d.Rename(context.Background(), &renameReq, d)
	if e, ok := err.(fuse.Errno); !ok || !os.IsNotExist(syscall.Errno(e)) {
		t.Errorf("Rename from a read-only directory gave error %T(%v), want fuseErrno(os.IsNotExist(err))", err, err)
	}
}

func TestDir_Rename_ReadWrite_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, false)
	if err := ioutil.WriteFile(src+"/a", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}
	renameReq := fuse.RenameRequest{OldName: "a", NewName: "b"}

	if err := d.Rename(context.Background(), &renameReq, d); err != fuseErrno(syscall.EPERM) {
		t.Errorf("Rename from a read-only directory gave error %T(%v), want fuseErrno(syscall.EPERM)", err, err)
	}
}

func TestDir_RenameInSameDir_ReadWrite_Ok(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)
	if err := ioutil.WriteFile(src+"/a", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}

	d := newDir(src, DevInoPair{}, true)
	req := fuse.RenameRequest{OldName: "a", NewName: "b"}

	if err := d.Rename(context.Background(), &req, d); err != nil {
		t.Errorf("Rename from a read/write directory gave error %T(%v), want nil", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.UnderlyingPath(), req.NewName)); err != nil {
		t.Errorf("Underlying filesystem gave error for renamed file: %v, want nil", err)
	}
}

func TestDir_RenameInDifferentDir_ReadWrite_Ok(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)
	dst := dirSetup(t)
	defer dirTeardown(dst, t)
	if err := ioutil.WriteFile(src+"/a", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}

	srcDir := newDir(src, DevInoPair{}, true)
	dstDir := newDir(dst, DevInoPair{}, true)

	req := fuse.RenameRequest{OldName: "a", NewName: "b"}
	if err := srcDir.Rename(context.Background(), &req, dstDir); err != nil {
		t.Errorf("Rename from a read/write directory gave error %T(%v), want nil", err, err)
	}
	if _, err := os.Stat(filepath.Join(dstDir.UnderlyingPath(), req.NewName)); err != nil {
		t.Errorf("Underlying filesystem gave error for renamed file: %v, want nil", err)
	}

	req = fuse.RenameRequest{OldName: "b", NewName: "a"}
	if err := dstDir.Rename(context.Background(), &req, srcDir); err != nil {
		t.Errorf("Rename from a read/write directory gave error %T(%v), want nil", err, err)
	}
	if _, err := os.Stat(filepath.Join(srcDir.UnderlyingPath(), req.NewName)); err != nil {
		t.Errorf("Underlying filesystem gave error for renamed file: %v, want nil", err)
	}
}

func TestDir_Remove_ReadOnly_Error(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	d := newDir(src, DevInoPair{}, false)
	if err := ioutil.WriteFile(src+"/c", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}
	removeReq := fuse.RemoveRequest{Name: "c"}

	if err := d.Remove(context.Background(), &removeReq); err != fuseErrno(syscall.EPERM) {
		t.Errorf("Remove from a read-only directory gave error %T(%v), want fuseErrno(syscall.EPERM)", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.underlyingPath, removeReq.Name)); err != nil {
		t.Errorf("Stat got error: %v, want nil", err)
	}
}

func TestDir_Remove_ReadWrite_Ok(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)
	if err := ioutil.WriteFile(src+"/a", []byte(""), 0644); err != nil {
		t.Fatal("WriteFile failed with error: ", err)
	}

	d := newDir(src, DevInoPair{}, true)
	req := fuse.RemoveRequest{Name: "a"}

	if err := d.Remove(context.Background(), &req); err != nil {
		t.Errorf("Remove call gave error %T(%v), want nil", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.UnderlyingPath(), req.Name)); fuseErrno(err) != fuse.Errno(syscall.ENOENT) {
		t.Errorf("Stat on removed file gave err: %v, want: %v", err, syscall.ENOENT)
	}
}

func TestDir_ReleaseTwice(t *testing.T) {
	src := dirSetup(t)
	defer dirTeardown(src, t)

	for _, writable := range []bool{false, true} {
		d := newDir(src, DevInoPair{}, writable)

		handle, err := d.Open(context.Background(), OpenRequestDir, nil)
		if err != nil {
			t.Error("Directory open failed with error: ", err)
		}
		if err := handle.(*OpenDir).Release(context.Background(), nil); err != nil {
			t.Error("Directory close failed with error: ", err)
		}
		err = handle.(*OpenDir).Release(context.Background(), nil)
		if _, ok := err.(fuse.Errno); !ok {
			t.Errorf("Closing a file twice gave error of type: %T, expected fuse.Errno", err)
		}
	}
}
