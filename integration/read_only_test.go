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
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadOnly_DirectoryStructure(t *testing.T) {
	state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.tearDown(t)

	mkdirAllOrFatal(t, filepath.Join(state.root, "dir1"), 0755)
	mkdirAllOrFatal(t, filepath.Join(state.root, "dir2"), 0500)
	mkdirAllOrFatal(t, filepath.Join(state.root, "dir3/dir1"), 0700)
	mkdirAllOrFatal(t, filepath.Join(state.root, "dir3/dir2"), 0755)

	for _, dir := range []string{"", "dir1", "dir2", "dir3/dir1", "dir3/dir2"} {
		if err := dirEquals(filepath.Join(state.root, dir), filepath.Join(state.mountPoint, dir)); err != nil {
			t.Error(err)
		}
	}
}

func TestReadOnly_FileContents(t *testing.T) {
	state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.tearDown(t)

	writeFileOrFatal(t, filepath.Join(state.root, "file"), 0400, "foo")
	mkdirAllOrFatal(t, filepath.Join(state.root, "dir1/dir2"), 0755)
	writeFileOrFatal(t, filepath.Join(state.root, "dir1/dir2/file"), 0600, "bar baz")

	// Do the checks many times to ensure file reads and handles do not conflict with each
	// other, and that we do not leak file descriptors within sandboxfs.
	for i := 0; i < 1000; i++ {
		if err := fileEquals(filepath.Join(state.mountPoint, "file"), "foo"); err != nil {
			t.Error(err)
		}
		if err := fileEquals(filepath.Join(state.mountPoint, "dir1/dir2/file"), "bar baz"); err != nil {
			t.Error(err)
		}
	}
}

func TestReadOnly_DeleteUnderlyingRoot(t *testing.T) {
	state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.tearDown(t)

	if _, err := ioutil.ReadDir(state.mountPoint); err != nil {
		t.Errorf("accessing the mount point should have succeeded, but got %v", err)
	}

	if err := os.RemoveAll(state.root); err != nil {
		t.Fatalf("failed to remove underlying root directory: %v", err)
	}

	if _, err := ioutil.ReadDir(state.mountPoint); err == nil {
		t.Errorf("accessing the mount point should have failed, but it did not")
	}
}

func TestReadOnly_ReplaceUnderlyingFile(t *testing.T) {
	state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.tearDown(t)

	externalFile := filepath.Join(state.root, "foo")
	internalFile := filepath.Join(state.mountPoint, "foo")

	writeFileOrFatal(t, externalFile, 0600, "old contents")
	if err := fileEquals(internalFile, "old contents"); err != nil {
		t.Fatalf("test file doesn't match expected contents: %v", err)
	}

	writeFileOrFatal(t, externalFile, 0600, "new contents")
	err := fileEquals(internalFile, "new contents")
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
	state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
	defer state.tearDown(t)

	mkdirAllOrFatal(t, filepath.Join(state.root, "first/a"), 0755)
	mkdirAllOrFatal(t, filepath.Join(state.root, "first/b"), 0755)
	mkdirAllOrFatal(t, filepath.Join(state.root, "first/c"), 0755)
	mkdirAllOrFatal(t, filepath.Join(state.root, "second/1"), 0755)

	if err := dirEquals(filepath.Join(state.root, "first"), filepath.Join(state.mountPoint, "first")); err != nil {
		t.Fatal(err)
	}
	if err := dirEquals(filepath.Join(state.root, "second"), filepath.Join(state.mountPoint, "second")); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(filepath.Join(state.root, "first"), filepath.Join(state.root, "third")); err != nil {
		t.Fatalf("failed to move underlying directory away: %v", err)
	}
	if err := os.Rename(filepath.Join(state.root, "second"), filepath.Join(state.root, "first")); err != nil {
		t.Fatalf("failed to replace previous underlying directory: %v", err)
	}

	if err := dirEquals(filepath.Join(state.root, "first"), filepath.Join(state.mountPoint, "first")); err != nil {
		t.Error(err)
	}
	if err := dirEquals(filepath.Join(state.root, "third"), filepath.Join(state.mountPoint, "third")); err != nil {
		t.Error(err)
	}
}

func TestReadOnly_TargetDoesNotExist(t *testing.T) {
	wantStderr := `Unable to init sandbox: mapping /: creating node for path "/non-existent" failed: lstat /non-existent: no such file or directory` + "\n"

	stdout, stderr, err := runAndWait(1, "static", "--read_only_mapping=/:/non-existent", "irrelevant-mount-point")
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > 0 {
		t.Errorf("got %s; want stdout to be empty", stdout)
	}
	if !matchesRegexp(wantStderr, stderr) {
		t.Errorf("got %s; want stderr to match %s", stderr, wantStderr)
	}
}

// TODO(jmmv): Must have tests to ensure that read-only mappings are, well, read only.
