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
	"io/ioutil"
	"os"
	"runtime"
	"syscall"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
	"github.com/bazelbuild/sandboxfs/internal/sandbox"
)

func TestReadOnly_DirectoryStructure(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)
	utils.MustMkdirAll(t, state.RootPath("dir2"), 0500)
	utils.MustMkdirAll(t, state.RootPath("dir3/dir1"), 0700)
	utils.MustMkdirAll(t, state.RootPath("dir3/dir2"), 0755)

	for _, dir := range []string{"", "dir1", "dir2", "dir3/dir1", "dir3/dir2"} {
		if err := utils.DirEquals(state.RootPath(dir), state.MountPath(dir)); err != nil {
			t.Error(err)
		}
	}
}

func TestReadOnly_FileContents(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("file"), 0400, "foo")
	utils.MustMkdirAll(t, state.RootPath("dir1/dir2"), 0755)
	utils.MustWriteFile(t, state.RootPath("dir1/dir2/file"), 0600, "bar baz")

	// Do the checks many times to ensure file reads and handles do not conflict with each
	// other, and that we do not leak file descriptors within sandboxfs.
	for i := 0; i < 1000; i++ {
		if err := utils.FileEquals(state.MountPath("file"), "foo"); err != nil {
			t.Error(err)
		}
		if err := utils.FileEquals(state.MountPath("dir1/dir2/file"), "bar baz"); err != nil {
			t.Error(err)
		}
	}
}

func TestReadOnly_DeleteUnderlyingRoot(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.TearDown(t)

	if _, err := ioutil.ReadDir(state.MountPath()); err != nil {
		t.Errorf("accessing the mount point should have succeeded, but got %v", err)
	}

	if err := os.RemoveAll(state.RootPath()); err != nil {
		t.Fatalf("failed to remove underlying root directory: %v", err)
	}

	if _, err := ioutil.ReadDir(state.MountPath()); err == nil {
		t.Errorf("accessing the mount point should have failed, but it did not")
	}
}

func TestReadOnly_ReplaceUnderlyingFile(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.TearDown(t)

	externalFile := state.RootPath("foo")
	internalFile := state.MountPath("foo")

	utils.MustWriteFile(t, externalFile, 0600, "old contents")
	if err := utils.FileEquals(internalFile, "old contents"); err != nil {
		t.Fatalf("test file doesn't match expected contents: %v", err)
	}

	utils.MustWriteFile(t, externalFile, 0600, "new contents")
	err := utils.FileEquals(internalFile, "new contents")
	// The behavior we get for this test on macOS and on Linux is different, and it is yet not
	// clear why that is.  In principle, Linux is right here, but let's also check the current
	// known behavior on macOS so that we can catch when it ever changes.
	// TODO(jmmv): Investigate and fix the inconsistency.
	switch runtime.GOOS {
	case "darwin":
		if err == nil {
			t.Fatalf("test file matches expected contents, but we know it shouldn't have on this platform")
		}
	case "linux":
		if err != nil {
			t.Fatalf("test file doesn't match expected contents: %v", err)
		}
	default:
		t.Fatalf("don't know how this test behaves in this platform")
	}
}

func TestReadOnly_MoveUnderlyingDirectory(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("first/a"), 0755)
	utils.MustMkdirAll(t, state.RootPath("first/b"), 0755)
	utils.MustMkdirAll(t, state.RootPath("first/c"), 0755)
	utils.MustMkdirAll(t, state.RootPath("second/1"), 0755)

	if err := utils.DirEquals(state.RootPath("first"), state.MountPath("first")); err != nil {
		t.Fatal(err)
	}
	if err := utils.DirEquals(state.RootPath("second"), state.MountPath("second")); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(state.RootPath("first"), state.RootPath("third")); err != nil {
		t.Fatalf("failed to move underlying directory away: %v", err)
	}
	if err := os.Rename(state.RootPath("second"), state.RootPath("first")); err != nil {
		t.Fatalf("failed to replace previous underlying directory: %v", err)
	}

	if err := utils.DirEquals(state.RootPath("first"), state.MountPath("first")); err != nil {
		t.Error(err)
	}
	if err := utils.DirEquals(state.RootPath("third"), state.MountPath("third")); err != nil {
		t.Error(err)
	}
}

func TestReadOnly_TargetDoesNotExist(t *testing.T) {
	wantStderr := `Unable to init sandbox: mapping /: creating node for path "/non-existent" failed: lstat /non-existent: no such file or directory` + "\n"

	stdout, stderr, err := utils.RunAndWait(1, "static", "--read_only_mapping=/:/non-existent", "irrelevant-mount-point")
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > 0 {
		t.Errorf("got %s; want stdout to be empty", stdout)
	}
	if !utils.MatchesRegexp(wantStderr, stderr) {
		t.Errorf("got %s; want stderr to match %s", stderr, wantStderr)
	}
}

func TestReadOnly_Attributes(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")
	utils.MustSymlink(t, "missing", state.RootPath("symlink"))

	for _, name := range []string{"dir", "file", "symlink"} {
		outerPath := state.RootPath(name)
		outerFileInfo, err := os.Lstat(outerPath)
		if err != nil {
			t.Fatalf("failed to stat %s: %v", outerPath, err)
		}
		outerStat := outerFileInfo.Sys().(*syscall.Stat_t)

		innerPath := state.MountPath(name)
		innerFileInfo, err := os.Lstat(innerPath)
		if err != nil {
			t.Fatalf("failed to stat %s: %v", innerPath, err)
		}
		innerStat := innerFileInfo.Sys().(*syscall.Stat_t)

		if innerFileInfo.Mode() != outerFileInfo.Mode() {
			t.Errorf("got mode %v for %s, want %v", innerFileInfo.Mode(), innerPath, outerFileInfo.Mode())
		}

		if sandbox.Atime(innerStat) != sandbox.Atime(outerStat) {
			t.Errorf("got atime %v for %s, want %v", sandbox.Atime(innerStat), innerPath, sandbox.Atime(outerStat))
		}
		if innerFileInfo.ModTime() != outerFileInfo.ModTime() {
			t.Errorf("got mtime %v for %s, want %v", innerFileInfo.ModTime(), innerPath, outerFileInfo.ModTime())
		}
		if sandbox.Ctime(innerStat) != sandbox.Ctime(outerStat) {
			t.Errorf("got ctime %v for %s, want %v", sandbox.Ctime(innerStat), innerPath, sandbox.Ctime(outerStat))
		}

		if innerStat.Nlink != outerStat.Nlink {
			t.Errorf("got nlink %v for %s, want %v", innerStat.Nlink, innerPath, outerStat.Nlink)
		}

		if innerStat.Rdev != outerStat.Rdev {
			t.Errorf("got rdev %v for %s, want %v", innerStat.Rdev, innerPath, outerStat.Rdev)
		}

		if innerStat.Blksize != outerStat.Blksize {
			t.Errorf("got blocksize %v for %s, want %v", innerStat.Blksize, innerPath, outerStat.Blksize)
		}
	}
}

// TODO(jmmv): Must have tests to ensure that read-only mappings are, well, read only.

// TODO(jmmv): Must have tests to verify that files are valid mapping targets, which is what we
// promise users in the documentation.
