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
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

func TestOptions_Allow(t *testing.T) {
	root := utils.RequireRoot(t, "Requires root privileges to spawn sandboxfs under different users")

	username := os.Getenv("UNPRIVILEGED_USER")
	if username == "" {
		t.Skipf("UNPRIVILEGED_USER not set; must contain the name of an unprivileged user with FUSE access")
	}
	user, err := utils.LookupUser(username)
	if err != nil {
		t.Fatalf("Failed to get details about unprivileged user %s: %v", username, err)
	}
	t.Logf("Using primary unprivileged user: %v", user)

	other, err := utils.LookupUserOtherThan(root.Username, user.Username)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Using secondary unprivileged user: %v", other)

	var allowRootWorks bool
	switch runtime.GOOS {
	case "darwin":
		allowRootWorks = true
	case "linux":
		allowRootWorks = false
	default:
		t.Fatalf("Don't know how this test behaves in this platform")
	}

	testData := []struct {
		name string

		allowFlag   string
		wantMountOk bool
		okUsers     []*utils.UnixUser
		notOkUsers  []*utils.UnixUser
	}{
		{"Default", "", true, []*utils.UnixUser{user}, []*utils.UnixUser{root, other}},
		{"Other", "--allow=other", true, []*utils.UnixUser{user, other, root}, []*utils.UnixUser{}},
		{"Root", "--allow=root", allowRootWorks, []*utils.UnixUser{user, root}, []*utils.UnixUser{other}},
		{"Self", "--allow=self", true, []*utils.UnixUser{user}, []*utils.UnixUser{root, other}},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			if !d.wantMountOk {
				tempDir, err := ioutil.TempDir("", "test")
				if err != nil {
					t.Fatalf("Failed to create temporary directory: %v", err)
				}
				defer os.RemoveAll(tempDir)

				_, stderr, err := utils.RunAndWait(1, d.allowFlag, "static", "-read_only_mapping=/:/", tempDir)
				if err != nil {
					t.Fatal(err)
				}
				if !utils.MatchesRegexp("known.*broken", stderr) {
					t.Errorf("Want error message to mention known brokenness; got %v", stderr)
				}
				return
			}

			args := make([]string, 0)
			if d.allowFlag != "" {
				args = append(args, d.allowFlag)
			}
			args = append(args, "static", "-read_only_mapping=/:%ROOT%")

			state := utils.MountSetupWithUser(t, user, args...)
			defer state.TearDown(t)

			utils.MustWriteFile(t, state.RootPath("file"), 0444, "")
			file := state.MountPath("file")

			for _, user := range d.okUsers {
				if err := utils.FileExistsAsUser(file, user); err != nil {
					t.Errorf("Failed to access mount point as user %s: %v", user.Username, err)
				}
			}

			for _, user := range d.notOkUsers {
				if err := utils.FileExistsAsUser(file, user); err == nil {
					t.Errorf("Was able to access mount point as user %s; want error", user.Username)
				}
			}
		})
	}
}

func TestOptions_Syntax(t *testing.T) {
	testData := []struct {
		name string

		args       []string
		wantStderr string
	}{
		{"AllowBadValue", []string{"--allow=foo"}, "foo.*must be one of.*other"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := utils.RunAndWait(2, d.args...)
			if err != nil {
				t.Fatal(err)
			}
			if len(stdout) > 0 {
				t.Errorf("Got %s; want stdout to be empty", stdout)
			}
			if !utils.MatchesRegexp(d.wantStderr, stderr) {
				t.Errorf("Got %s; want stderr to match %s", stderr, d.wantStderr)
			}
			if !utils.MatchesRegexp("--help", stderr) {
				t.Errorf("Got %s; want --help mention in stderr", stderr)
			}
		})
	}
}
