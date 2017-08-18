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

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The tests in this file verify the read/write mapping.  In principle, they should ensure that the
// mapping is fully-functional, including in its read-only operations.  However, as we know that
// read/write mappings are implemented in the same way as read-only mappings, we "cheat" and only
// test here for the write-specific behaviors.

func TestReadWrite_CreateFile(t *testing.T) {
	state := mountSetup(t, "static", "-read_write_mapping=/:%ROOT%")
	defer state.tearDown(t)

	writeFileOrFatal(t, filepath.Join(state.root, "file"), 0644, "original content")
	mkdirAllOrFatal(t, filepath.Join(state.root, "subdir"), 0755)
	writeFileOrFatal(t, filepath.Join(state.mountPoint, "subdir/file"), 0644, "new content")

	if err := fileEquals(filepath.Join(state.mountPoint, "file"), "original content"); err != nil {
		t.Error(err)
	}
	if err := fileEquals(filepath.Join(state.mountPoint, "subdir/file"), "new content"); err != nil {
		t.Error(err)
	}
}

func TestReadWrite_RewriteFile(t *testing.T) {
	state := mountSetup(t, "static", "-read_write_mapping=/:%ROOT%")
	defer state.tearDown(t)

	writeFileOrFatal(t, filepath.Join(state.root, "file"), 0644, "original content")
	if err := fileEquals(filepath.Join(state.mountPoint, "file"), "original content"); err != nil {
		t.Error(err)
	}

	writeFileOrFatal(t, filepath.Join(state.mountPoint, "file"), 0644, "rewritten content")
	if err := fileEquals(filepath.Join(state.mountPoint, "file"), "rewritten content"); err != nil {
		t.Error(err)
	}
}

func TestReadWrite_RewriteFileWithShorterContent(t *testing.T) {
	state := mountSetup(t, "static", "-read_write_mapping=/:%ROOT%")
	defer state.tearDown(t)

	writeFileOrFatal(t, filepath.Join(state.mountPoint, "file"), 0644, "very long contents")
	writeFileOrFatal(t, filepath.Join(state.mountPoint, "file"), 0644, "short")
	// TODO(jmmv): There is a bug somewhere that is causing short writes over a long file to
	// not discard old data, or ignoring the truncate file request (which ioutil.WriteFile is
	// issuing).  Track and fix.
	bogusContents := "shortlong contents"
	if err := fileEquals(filepath.Join(state.mountPoint, "file"), bogusContents); err != nil {
		t.Error(err)
	}
}

// equivalentStats compares two os.FileInfo objects and returns nil if they represent the same
// file; otherwise returns a descriptive error including the differences between the two.
// This equivalency is to be used during file move tess, to check if a file was actually moved
// instead of recreated.
func equivalentStats(stat1 os.FileInfo, stat2 os.FileInfo) error {
	ino1 := stat1.Sys().(*syscall.Stat_t).Ino
	ino2 := stat2.Sys().(*syscall.Stat_t).Ino

	if stat1.Mode() != stat2.Mode() || stat1.ModTime() != stat2.ModTime() || ino1 != ino2 {
		return fmt.Errorf("got mode=%v, mtime=%v, inode=%v; want mode=%v, mtime=%v, inode=%v", stat1.Mode(), stat1.ModTime(), ino1, stat2.Mode(), stat2.ModTime(), ino2)
	}
	return nil
}

// doRenameTest is a helper function for the tests that verify the file system-level rename
// operation.  This takes the path of a file to be moved (the "old outer path"), the path of the
// rename target (the "new outer path"), and the corresponding paths within the mount point.
//
// Tests calling this function should only start a sandboxfs instance with the desired configuration
// and then immediately call this function.
func doRenameTest(t *testing.T, oldOuterPath, newOuterPath, oldInnerPath, newInnerPath string) {
	mkdirAllOrFatal(t, filepath.Dir(oldOuterPath), 0755)
	mkdirAllOrFatal(t, filepath.Dir(newOuterPath), 0755)
	mkdirAllOrFatal(t, filepath.Dir(oldInnerPath), 0755)
	mkdirAllOrFatal(t, filepath.Dir(newInnerPath), 0755)
	writeFileOrFatal(t, oldOuterPath, 0644, "some content")

	lstatOrFatal := func(path string) os.FileInfo {
		stat, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("failed to lstat %s: %v", path, err)
		}
		return stat
	}
	oldOuterStat := lstatOrFatal(oldOuterPath)
	oldInnerStat := lstatOrFatal(oldInnerPath)
	if err := os.Rename(oldInnerPath, newInnerPath); err != nil {
		t.Fatalf("failed to rename %s to %s: %v", oldInnerPath, newInnerPath, err)
	}
	newOuterStat := lstatOrFatal(newOuterPath)
	newInnerStat := lstatOrFatal(newInnerPath)

	if _, err := os.Lstat(oldOuterPath); os.IsExist(err) {
		t.Fatalf("old file name in root still present but should have disappeared: %s", oldOuterPath)
	}
	if _, err := os.Lstat(oldInnerPath); os.IsExist(err) {
		t.Fatalf("old file name in mount point still present but should have disappeared: %s", oldInnerPath)
	}
	if err := fileEquals(newOuterPath, "some content"); err != nil {
		t.Fatalf("new file name in root missing or with bad contents: %s: %v", newOuterPath, err)
	}
	if err := fileEquals(newInnerPath, "some content"); err != nil {
		t.Fatalf("new file name in mount point missing or with bad contents: %s: %v", newInnerPath, err)
	}

	if err := equivalentStats(oldOuterStat, newOuterStat); err != nil {
		t.Errorf("stats for %s and %s differ: %v", oldOuterPath, newOuterPath, err)
	}
	if err := equivalentStats(oldInnerStat, newInnerStat); err != nil {
		t.Errorf("stats for %s and %s differ: %v", oldInnerPath, newInnerPath, err)
	}
}

func TestReadWrite_RenameFile(t *testing.T) {
	state := mountSetup(t, "static", "-read_write_mapping=/:%ROOT%")
	defer state.tearDown(t)

	oldOuterPath := filepath.Join(state.root, "old-name")
	newOuterPath := filepath.Join(state.root, "new-name")
	oldInnerPath := filepath.Join(state.mountPoint, "old-name")
	newInnerPath := filepath.Join(state.mountPoint, "new-name")
	doRenameTest(t, oldOuterPath, newOuterPath, oldInnerPath, newInnerPath)
}

func TestReadWrite_MoveFile(t *testing.T) {
	state := mountSetup(t, "static", "-read_write_mapping=/:%ROOT%")
	defer state.tearDown(t)

	oldOuterPath := filepath.Join(state.root, "dir1/dir2/old-name")
	newOuterPath := filepath.Join(state.root, "dir2/dir3/dir4/new-name")
	oldInnerPath := filepath.Join(state.mountPoint, "dir1/dir2/old-name")
	newInnerPath := filepath.Join(state.mountPoint, "dir2/dir3/dir4/new-name")
	doRenameTest(t, oldOuterPath, newOuterPath, oldInnerPath, newInnerPath)
}
