// Copyright 2019 Google Inc.
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

func TestProfiling_OptionalSupport(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	profile := filepath.Join(tempDir, "cpu.prof")
	arg := "--cpu_profile=" + profile

	if _, ok := utils.GetConfig().Features["profiling"]; ok {
		state := utils.MountSetup(t, arg, "--mapping=ro:/:%ROOT%")
		// Explicitly stop sandboxfs (which is different to what most other tests do).
		// We need to do this here to cause the profiles to be written to disk.
		state.TearDown(t)

		// Check if the profile exists and is not empty.  We cannot do much more complex
		// verifications here, but ensuring the file is not empty is sufficient to verify
		// that the profiles were actually written during termination.
		stat, err := os.Lstat(profile)
		if err != nil {
			t.Fatalf("Cannot find expected profile %s", profile)
		}
		if stat.Size() == 0 {
			t.Errorf("Expected profile %s is empty", profile)
		}
	} else {
		_, stderr, err := utils.RunAndWait(1, arg, filepath.Join(tempDir, "root"))
		if err != nil {
			t.Fatal(err)
		}
		wantStderr := "Failed to start CPU profile.*feature not enabled"
		if !utils.MatchesRegexp(wantStderr, stderr) {
			t.Errorf("Got %s; want stderr to match %s", stderr, wantStderr)
		}
	}
}
