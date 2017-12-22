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

package utils

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

// TODO(jmmv): All functions in this file should use t.Helper(), but we must first be ready to
// switch to Go 1.9 externally.

// MustMkdirAll wraps os.MkdirAll and immediately fails the test case on failure.
// This is purely syntactic sugar to keep test setup short and concise.
func MustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("Failed to create directory %s: %v", path, err)
	}
}

// MustSymlink wraps os.Symlink and immediately fails the test case on failure.
// This is purely syntactic sugar to keep test setup short and concise.
//
// Note that, compared to the other *OrFatal operations, this one does not take file permissions
// into account because Linux does not have an lchmod(2) system call, nor Go offers a mechanism to
// call it on the systems that support it.
func MustSymlink(t *testing.T, target string, path string) {
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Failed to create symlink %s: %v", path, err)
	}
}

// MustWriteFile wraps ioutil.WriteFile and immediately fails the test case on failure.
// This is purely syntactic sugar to keep test setup short and concise.
func MustWriteFile(t *testing.T, path string, perm os.FileMode, contents string) {
	if err := ioutil.WriteFile(path, []byte(contents), perm); err != nil {
		t.Fatalf("Failed to create file %s: %v", path, err)
	}
}

// RequireRoot checks if the test is running as root and skips the test with the given reason
// otherwise.
func RequireRoot(t *testing.T, skipReason string) *UnixUser {
	if os.Getuid() != 0 {
		t.Skipf(skipReason)
	}
	root, err := LookupUID(os.Getuid())
	if err != nil {
		t.Fatalf("Failed to get details about root user: %v", err)
	}
	t.Logf("Running test as: %v", root)
	return root
}

// DirEquals checks if the contents of two directories are the same.  The equality check is based
// on the directory entry names and their modes.
func DirEquals(path1 string, path2 string) error {
	names := make([]map[string]os.FileMode, 2)
	for i, path := range []string{path1, path2} {
		dirents, err := ioutil.ReadDir(path)
		if err != nil {
			return fmt.Errorf("failed to read contents of directory %s: %v", path, err)
		}
		names[i] = make(map[string]os.FileMode, len(dirents))
		for _, dirent := range dirents {
			names[i][dirent.Name()] = dirent.Mode()
		}
	}
	if !reflect.DeepEqual(names[0], names[1]) {
		return fmt.Errorf("contents of directory %s do not match %s; got %v, want %v", path1, path2, names[1], names[0])
	}
	return nil
}

// DirEntryNamesEqual checks if the names of the entries in the given directory match the expected
// names in the given slice.  The list of expected entries needs to be sorted alphabetically.
func DirEntryNamesEqual(path string, wantNames []string) error {
	dirents, err := ioutil.ReadDir(path)
	if err != nil {
		return fmt.Errorf("failed to read contents of directory %s: %v", path, err)
	}

	var names []string
	for _, dirent := range dirents {
		names = append(names, dirent.Name())
	}
	sort.Strings(names)

	if !reflect.DeepEqual(names, wantNames) {
		return fmt.Errorf("got entries %v for directory %s; want %v", names, path, wantNames)
	}

	return nil
}

// FileEquals checks if a file matches the expected contents.
func FileEquals(path string, wantContents string) error {
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}
	if string(contents) != wantContents {
		return fmt.Errorf("file %s doesn't match expected contents: got '%s', want '%s'", path, contents, wantContents)
	}
	return nil
}

// FileExistsAsUser checks if the given path is accessible by the given user.  The user may be nil,
// in which case the current user is assumed.
func FileExistsAsUser(path string, user *UnixUser) error {
	// We cannot do the access test in-process by switching the effective UID/GID because we
	// must fully drop privileges to the given user.  If we dropped privileges, we wouldn't be
	// able to restore them to root's for the remainder of the test.
	//
	// Also, using os.Lstat or running "test -e" is insufficient: FUSE still allows root to
	// traverse the file system (that is, to resolve nodes and even stat them) even if root has
	// not been granted access through the allow_other/allow_root options.  Therefore, we must
	// read file contents to really validate the access control.  Note that this might be a bug
	// in OSXFUSE.
	cmd := exec.Command("cat", path)
	SetCredential(cmd, user)
	return cmd.Run()
}

// MatchesRegexp returns true if the given string s matches the pattern.
func MatchesRegexp(pattern string, s string) bool {
	match, err := regexp.MatchString(pattern, s)
	if err != nil {
		// This function is intended to be used exclusively from tests, and as such we know
		// that the given pattern must be valid.  If it's not, we've got a bug in the code
		// that must be fixed: there is no point in returning this as an error.
		panic(fmt.Sprintf("invalid regexp %s: %v; this is a bug in the test code", pattern, err))
	}
	return match
}
