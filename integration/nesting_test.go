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
	"testing"
)

func TestNesting_VirtualIntermediateComponents(t *testing.T) {
	state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%", "-read_only_mapping=/1/2/3/4/5:%ROOT%/subdir")
	defer state.tearDown(t)

	writeFileOrFatal(t, filepath.Join(state.root, "subdir", "file"), 0644, "some contents")

	if err := dirEquals(filepath.Join(state.root, "subdir"), filepath.Join(state.mountPoint, "1/2/3/4/5")); err != nil {
		t.Error(err)
	}
	if err := fileEquals(filepath.Join(state.mountPoint, "1/2/3/4/5", "file"), "some contents"); err != nil {
		t.Error(err)
	}

	mkdirAllOrFatal(t, filepath.Join(state.tempDir, "golden/1/2/3/4/5"), 0755)
	for _, dir := range []string{"1/2/3/4", "1/2/3", "1/2", "1"} {
		goldenDir := filepath.Join(state.tempDir, "golden", dir)
		if err := os.Chmod(goldenDir, 0555); err != nil {
			t.Errorf("failed to set golden dir permissions to 0555 to match virtual dir expectations: %v", err)
		}
		defer os.Chmod(goldenDir, 0755) // To allow cleanup in tearDown to succeed.

		virtualDir := filepath.Join(state.mountPoint, dir)
		if err := dirEquals(goldenDir, virtualDir); err != nil {
			t.Error(err)
		}
	}
}

func TestNesting_ReadWriteWithinReadOnly(t *testing.T) {
	state := mountSetup(t, "static", "-read_write_mapping=/:%ROOT%", "-read_only_mapping=/ro:%ROOT%/one/two", "-read_write_mapping=/ro/rw:%ROOT%")
	defer state.tearDown(t)

	if err := os.MkdirAll(filepath.Join(state.mountPoint, "ro/hello"), 0755); err == nil {
		t.Errorf("mkdir succeeded in read-only mapping")
	}
	if err := os.MkdirAll(filepath.Join(state.mountPoint, "ro/rw/hello"), 0755); err != nil {
		t.Errorf("mkdir failed in read-write mapping: %v", err)
	}
}

func TestNesting_PreserveSymlinks(t *testing.T) {
	state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%", "-read_only_mapping=/dir1/dir2:%ROOT%")
	defer state.tearDown(t)

	writeFileOrFatal(t, filepath.Join(state.root, "file"), 0644, "file in root directory")
	mkdirAllOrFatal(t, filepath.Join(state.root, "dir"), 0755)
	if err := os.Symlink("..", filepath.Join(state.root, "dir/up")); err != nil {
		t.Fatalf("failed to create test symlink: %v", err)
	}

	if err := fileEquals(filepath.Join(state.mountPoint, "dir/up/file"), "file in root directory"); err != nil {
		t.Error(err)
	}
	if err := fileEquals(filepath.Join(state.mountPoint, "dir1/dir2/dir/up/file"), "file in root directory"); err != nil {
		t.Error(err)
	}
}
