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
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

func TestLayout_MountPointDoesNotExist(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	mountPoint := filepath.Join(tempDir, "non-existent")
	wantStderr := "unable to mount: " + mountPoint + " does not exist\n"

	stdout, stderr, err := utils.RunAndWait(1, "static", "--mapping=ro:/:"+tempDir, mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > 0 {
		t.Errorf("Got %s; want stdout to be empty", stdout)
	}
	if !utils.MatchesRegexp(wantStderr, stderr) {
		t.Errorf("Got %s; want stderr to match %s", stderr, wantStderr)
	}
}

func TestLayout_RootMustBeDirectory(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	file := filepath.Join(tempDir, "file")
	utils.MustWriteFile(t, file, 0644, "")

	wantStderr := "unable to init sandbox: can't map a file at root; must be a directory\n"

	stdout, stderr, err := utils.RunAndWait(1, "static", "--mapping=ro:/:"+file, "irrelevant-mount-point")
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > 0 {
		t.Errorf("Got %s; want stdout to be empty", stdout)
	}
	if !utils.MatchesRegexp(wantStderr, stderr) {
		t.Errorf("Got %s; want stderr to match %s", stderr, wantStderr)
	}
}

func TestLayout_DuplicateMapping(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	wantStderr := "unable to init sandbox: mapping /a/a: two nodes mapped at the same location\n"

	stdout, stderr, err := utils.RunAndWait(1, "static", "--mapping=ro:/:"+tempDir, "--mapping=ro:/a/a:"+tempDir, "--mapping=ro:/a/b:"+tempDir, "--mapping=ro:/a/a:/does-not-matter", "irrelevant-mount-point")
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > 0 {
		t.Errorf("Got %s; want stdout to be empty", stdout)
	}
	if !utils.MatchesRegexp(wantStderr, stderr) {
		t.Errorf("Got %s; want stderr to match %s", stderr, wantStderr)
	}
}

func TestLayout_FileMappedOnScaffoldDirectory(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	file := filepath.Join(tempDir, "file")
	utils.MustWriteFile(t, file, 0644, "")

	wantStderr := "unable to init sandbox: mapping /a: file \"" + file + "\" mapped over existing directory\n"

	stdout, stderr, err := utils.RunAndWait(1, "static", "--mapping=ro:/a/b/c:"+tempDir, "--mapping=ro:/a:"+file, "irrelevant-mount-point")
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > 0 {
		t.Errorf("Got %s; want stdout to be empty", stdout)
	}
	if !utils.MatchesRegexp(wantStderr, stderr) {
		t.Errorf("Got %s; want stderr to match %s", stderr, wantStderr)
	}
}
