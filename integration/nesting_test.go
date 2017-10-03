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
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

func TestNesting_VirtualIntermediateComponents(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%", "-read_only_mapping=/1/2/3/4/5:%ROOT%/subdir")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("subdir", "file"), 0644, "some contents")

	if err := utils.DirEquals(state.RootPath("subdir"), state.MountPath("1/2/3/4/5")); err != nil {
		t.Error(err)
	}
	if err := utils.FileEquals(state.MountPath("1/2/3/4/5", "file"), "some contents"); err != nil {
		t.Error(err)
	}

	utils.MustMkdirAll(t, state.TempPath("golden/1/2/3/4/5"), 0755)
	for _, dir := range []string{"1/2/3/4", "1/2/3", "1/2", "1"} {
		goldenDir := state.TempPath("golden", dir)
		if err := os.Chmod(goldenDir, 0555); err != nil {
			t.Errorf("failed to set golden dir permissions to 0555 to match virtual dir expectations: %v", err)
		}
		defer os.Chmod(goldenDir, 0755) // To allow cleanup in tearDown to succeed.

		virtualDir := state.MountPath(dir)
		if err := utils.DirEquals(goldenDir, virtualDir); err != nil {
			t.Error(err)
		}
	}
}

func TestNesting_ReadWriteWithinReadOnly(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_write_mapping=/:%ROOT%", "-read_only_mapping=/ro:%ROOT%/one/two", "-read_write_mapping=/ro/rw:%ROOT%")
	defer state.TearDown(t)

	if err := os.MkdirAll(state.MountPath("ro/hello"), 0755); err == nil {
		t.Errorf("mkdir succeeded in read-only mapping")
	}
	if err := os.MkdirAll(state.MountPath("ro/rw/hello"), 0755); err != nil {
		t.Errorf("mkdir failed in read-write mapping: %v", err)
	}
}

func TestNesting_SameTarget(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%", "-read_write_mapping=/dir1:%ROOT%/same", "-read_write_mapping=/dir2/dir3/dir4:%ROOT%/same")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.MountPath("dir1/file"), 0644, "old contents")
	utils.MustWriteFile(t, state.MountPath("dir2/dir3/dir4/file"), 0644, "new contents")

	externalDir := state.RootPath("same")
	if err := utils.FileEquals(filepath.Join(externalDir, "file"), "new contents"); err != nil {
		t.Error(err)
	}
	for _, dir := range []string{"/dir1", "/dir2/dir3/dir4"} {
		internalDir := state.MountPath(dir)
		if err := utils.DirEquals(externalDir, internalDir); err != nil {
			t.Error(err)
		}
	}

	var wantDir1FileContents string
	switch runtime.GOOS {
	case "darwin":
		// On macOS, FUSE does not seem to invalidate cached file contents at open(2) time.
		// The auto_cache mount-time option can be provided to discard the contents when the
		// file's mtime is modified, but that's subject to the node's validity period (the
		// default of one minute).  There is no kernel_cache mount-time option as exists on
		// Linux, which implies that the default behavior is undefined and cannot be
		// changed.  As a result, the contents of dir1/file remain stale even after
		// rewriting the file through dir2/dir3/dir4/file.
		//
		// This test could still be faulty and subject to OSXFUSE implementation details.
		// However, it's worth keeping: if the underlying behavior changes, we want to know
		// so that we can think on how to address the problem.
		//
		// TODO(jmmv): This is unfortunate and an undesirable inconsistency with Linux so
		// we should find a way to homogenize the behavior.
		wantDir1FileContents = "old contents"
	case "linux":
		// On Linux, FUSE invalidates any cached file contents at open(2) time unless the
		// kernel_cache mount-time option is provided.  This means that through dir1/file we
		// can see the contents that were written through dir2/dir3/dir4/file.  This is the
		// desirable behavior for sandboxfs as it makes it more reliable against external
		// file content changes and makes the duplicate mapping behavior sane.
		wantDir1FileContents = "new contents"
	default:
		t.Fatalf("don't know how this test behaves in this platform")
	}
	if err := utils.FileEquals(state.MountPath("dir1/file"), wantDir1FileContents); err != nil {
		t.Error(err)
	}

	if err := utils.FileEquals(state.MountPath("dir2/dir3/dir4/file"), "new contents"); err != nil {
		t.Error(err)
	}
}

func TestNesting_PreserveSymlinks(t *testing.T) {
	state := utils.MountSetup(t, "static", "-read_only_mapping=/:%ROOT%", "-read_only_mapping=/dir1/dir2:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("file"), 0644, "file in root directory")
	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	if err := os.Symlink("..", state.RootPath("dir/up")); err != nil {
		t.Fatalf("failed to create test symlink: %v", err)
	}

	if err := utils.FileEquals(state.MountPath("dir/up/file"), "file in root directory"); err != nil {
		t.Error(err)
	}
	if err := utils.FileEquals(state.MountPath("dir1/dir2/dir/up/file"), "file in root directory"); err != nil {
		t.Error(err)
	}
}
