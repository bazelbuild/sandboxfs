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

package shell

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
)

// checkError is a helper function for tests that want to check that a specific error is raised,
// and does so by ensuring that the given error matches the given regexp.
func checkError(original error, pattern string) error {
	if original == nil {
		return fmt.Errorf("want failure that matches %s, got none", pattern)
	}
	match, err := regexp.MatchString(pattern, original.Error())
	if err != nil {
		return fmt.Errorf("bad regexp %s: this is a bug in the test", pattern)
	}
	if !match {
		return fmt.Errorf("want failure that matches %s, got %v", pattern, original)
	}
	return nil
}

func TestGrep(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	goodFile := filepath.Join(tempDir, "file")
	if err := ioutil.WriteFile(goodFile, []byte("foooobar baZ"), 0400); err != nil {
		t.Fatalf("Failed to create test file %s: %v", goodFile, err)
	}

	badFile := filepath.Join(tempDir, "missing")

	testData := []struct {
		name string

		pattern          string
		file             string
		wantMatch        bool
		wantErrorPattern string
	}{
		{"Match", `fo+bar`, goodFile, true, ""},
		{"NoMatch", `baz`, goodFile, false, ""},

		{"MissingFile", ``, badFile, false, "failed to open " + badFile},
		{"BadRegexp", `+foo`, goodFile, false, "failed to search for \\+foo in " + goodFile},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			match, err := Grep(d.pattern, d.file)
			if d.wantErrorPattern == "" {
				if err != nil {
					t.Fatalf("Grep failed: %v", err)
				}

				if match != d.wantMatch {
					t.Errorf("Got match %v, want %v", match, d.wantMatch)
				}
			} else {
				if err := checkError(err, d.wantErrorPattern); err != nil {
					t.Fatal(err) // Message returned by helper is sufficient.
				}
			}
		})
	}
}

func TestInstall(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	contents := "original text"
	goodSource := filepath.Join(tempDir, "file1")
	if err := ioutil.WriteFile(goodSource, []byte(contents), 0444); err != nil {
		t.Fatalf("Failed to create test file %s: %v", goodSource, err)
	}

	badSource := filepath.Join(tempDir, "missing")

	newTarget := filepath.Join(tempDir, "file2")

	existingTarget := filepath.Join(tempDir, "file3")
	if err := ioutil.WriteFile(existingTarget, []byte("old text"), 0600); err != nil {
		t.Fatalf("Failed to create test file %s: %v", existingTarget, err)
	}

	badTarget := filepath.Join(tempDir, "missing-subdir/file")

	// Set a rather unusual umask throughout the test to ensure that our Install function does
	// honor the permissions we pass it on any created files.
	oldmask := syscall.Umask(0047)
	defer syscall.Umask(oldmask)

	testData := []struct {
		name string

		source           string
		target           string
		mode             os.FileMode
		wantErrorPattern string
	}{
		{"NewTarget", goodSource, newTarget, 0644, ""},
		{"ReplaceTarget", goodSource, existingTarget, 0400, ""},

		{"CannotOpenSource", badSource, "", 0000, "cannot open " + badSource + " for read"},
		{"CannotOpenTarget", goodSource, badTarget, 0000, "cannot open " + badTarget + " for write"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			err := Install(d.source, d.target, d.mode)
			if d.wantErrorPattern == "" {
				if err != nil {
					t.Fatalf("Install failed: %v", err)
				}

				fileInfo, err := os.Lstat(d.target)
				if err != nil {
					t.Fatalf("Cannot stat created target %s: %v", d.target, err)
				}
				if fileInfo.Mode() != d.mode {
					t.Errorf("Installed file permissions are wrong: got %v, want %v", fileInfo.Mode(), d.mode)
				}

				readContents, err := ioutil.ReadFile(d.target)
				if err != nil {
					t.Fatalf("Cannot read created target %s: %v", d.target, err)
				}
				if string(readContents) != contents {
					t.Errorf("Installed file does not match original file: got %s, want %s", readContents, contents)
				}
			} else {
				if err := checkError(err, d.wantErrorPattern); err != nil {
					t.Fatal(err) // Message returned by helper is sufficient.
				}
			}
		})
	}
}
