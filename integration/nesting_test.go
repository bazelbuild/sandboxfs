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
	"reflect"
	"syscall"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

func TestNesting_ScaffoldIntermediateComponents(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%", "--mapping=ro:/1/2/3/4/5:%ROOT%/subdir")
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
			t.Errorf("Failed to set golden dir permissions to 0555 to match scaffold dir expectations: %v", err)
		}
		defer os.Chmod(goldenDir, 0755) // To allow cleanup in tearDown to succeed.

		scaffoldDir := state.MountPath(dir)
		if err := utils.DirEquals(goldenDir, scaffoldDir); err != nil {
			t.Error(err)
		}
	}
}

func TestNesting_ScaffoldIntermediateComponentsAreImmutable(t *testing.T) {
	// Scaffold directories have mode 0555 to signal that they are read-only.  The mode alone
	// prevents unprivileged users from writing to those directories, but the mode has no effect
	// on root accesses.  Therefore, run the test as root to bypass permission checks and
	// attempt real writes.
	root := utils.RequireRoot(t, "Requires root privileges to write to directories with mode 0555")

	state := utils.MountSetupWithUser(t, root, "--mapping=ro:/:%ROOT%", "--mapping=rw:/1/2/3:%ROOT%/subdir")
	defer state.TearDown(t)

	for _, dir := range []string{"1/foo", "1/2/foo"} {
		err := os.Mkdir(state.MountPath(dir), 0755)
		pathErr, ok := err.(*os.PathError)
		if !ok || pathErr.Err != syscall.EPERM {
			t.Errorf("Want Mkdir to fail inside scaffold directory %s with %v; got %v (%v)", dir, syscall.EPERM, err, reflect.TypeOf(err))
		}
	}
	if err := os.Mkdir(state.MountPath("1/2/3/foo"), 0755); err != nil {
		t.Errorf("Want Mkdir to succeed inside non-scaffold directory; got %v", err)
	}
}

func TestNesting_ReadWriteWithinReadOnly(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%", "--mapping=ro:/ro:%ROOT%/one/two", "--mapping=rw:/ro/rw:%ROOT%")
	defer state.TearDown(t)

	if err := os.MkdirAll(state.MountPath("ro/hello"), 0755); err == nil {
		t.Errorf("Mkdir succeeded in read-only mapping")
	}
	if err := os.MkdirAll(state.MountPath("ro/rw/hello"), 0755); err != nil {
		t.Errorf("Mkdir failed in read-write mapping: %v", err)
	}
}

func TestNesting_SameTarget(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%", "--mapping=rw:/dir1:%ROOT%/same", "--mapping=rw:/dir2/dir3/dir4:%ROOT%/same")
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

	// We share the same internal representation for different mappings that point to the same
	// underlying file, which means that we can assume content changes through a mapping will be
	// reflected on the other mapping.  This is independent of how the kernel caches work or
	// when content invalidations happen.
	if err := utils.FileEquals(state.MountPath("dir1/file"), "new contents"); err != nil {
		t.Error(err)
	}
	if err := utils.FileEquals(state.MountPath("dir2/dir3/dir4/file"), "new contents"); err != nil {
		t.Error(err)
	}
}

func TestNesting_PreserveSymlinks(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%", "--mapping=ro:/dir1/dir2:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("file"), 0644, "file in root directory")
	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	if err := os.Symlink("..", state.RootPath("dir/up")); err != nil {
		t.Fatalf("Failed to create test symlink: %v", err)
	}

	if err := utils.FileEquals(state.MountPath("dir/up/file"), "file in root directory"); err != nil {
		t.Error(err)
	}
	if err := utils.FileEquals(state.MountPath("dir1/dir2/dir/up/file"), "file in root directory"); err != nil {
		t.Error(err)
	}
}
