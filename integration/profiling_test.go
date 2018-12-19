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
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

// listenAddressRegex captures the host:port pair on which the status server started by sandboxfs's
// --listen_address flag is listening on.  This is useful when the port is specified as ":0", which
// causes sandboxfs to allocate an unused port.
var listenAddressRegex = regexp.MustCompile(`starting HTTP server on ([^:]+:\d+)`)

func TestProfiling_Http(t *testing.T) {
	stderr := new(bytes.Buffer)
	state := utils.MountSetupWithOutputs(t, nil, stderr, "--listen_address=localhost:0", "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	matches := listenAddressRegex.FindStringSubmatch(stderr.String())
	if matches == nil {
		t.Fatalf("Cannot determine address on which server was started; log was: %v", stderr)
	} else if len(matches) != 2 {
		t.Fatalf("Multiple matches while trying to determine server's address; log was: %v", stderr)
	}
	hostPort := matches[1]

	// Ideally we would request a valid profile, not an unknown one, and verify that the output is
	// valid in some way.  But, for now, just checking that the endpoints exist and behave as
	// expected is sufficient.
	response, err := http.Get(fmt.Sprintf("http://%s/debug/pprof/fakename", hostPort))
	if err != nil {
		t.Fatalf("Failed to query fake pprof endpoint: %v", err)
	}
	defer response.Body.Close()

	contents, err := ioutil.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("Failed to read response from fake pprof endpoint: %v", err)
	}
	wantContents := "Unknown profile.*"
	if !utils.MatchesRegexp(wantContents, string(contents)) {
		t.Errorf("Got unexpected pprof response %s; want %s", string(contents), wantContents)
	}
}

func TestProfiling_FileProfiles(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	testData := []struct {
		name string

		args         []string
		wantProfiles []string
	}{
		{"CpuProfile", []string{"--cpu_profile=" + tempDir + "/cpu.prof"}, []string{"cpu.prof"}},
		{"MemProfile", []string{"--mem_profile=" + tempDir + "/mem.prof"}, []string{"mem.prof"}},
		{"CpuAndMemProfile", []string{"--cpu_profile=" + tempDir + "/cpu2.prof", "--mem_profile=" + tempDir + "/mem2.prof"}, []string{"cpu2.prof", "mem2.prof"}},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			state := utils.MountSetup(t, append(d.args, "--mapping=ro:/:%ROOT%")...)
			// Explicitly stop sandboxfs (which is different to what most other tests do).  We need
			// to do this here to cause the profiles to be written to disk.
			state.TearDown(t)

			for _, profile := range d.wantProfiles {
				// Check if the profile exists and is not empty.  We cannot do much more complex
				// verifications here, but ensuring the file is not empty is sufficient to verify
				// that the profiles were actually written during termination.
				stat, err := os.Lstat(filepath.Join(tempDir, profile))
				if err != nil {
					t.Errorf("Cannot find expected profile %s", profile)
					continue
				}
				if stat.Size() == 0 {
					t.Errorf("Expected profile %s is empty", profile)
				}
				os.Remove(filepath.Join(tempDir, profile))
			}
		})
	}
}

func TestProfiling_BadConfiguration(t *testing.T) {
	incompatibleSettings := "invalid profiling settings: file-based CPU or memory profiling are incompatible with a listening address\n"

	testData := []struct {
		name string

		args         []string
		wantExitCode int
		wantStderr   string
	}{
		{"BadCpuFile", []string{"--cpu_profile=/tmp"}, 1, "failed to create CPU profile /tmp: .*"},
		{"BadMemFile", []string{"--mem_profile=/tmp"}, 1, "failed to create memory profile /tmp: .*"},

		{"BadListenAddress", []string{"--listen_address=foo:bar"}, 1, "failed to start HTTP server: .*"},

		{"CpuAndListenAddress", []string{"--cpu_profile=foo", "--listen_address=host:1234"}, 2, incompatibleSettings},
		{"MemAndListenAddress", []string{"--mem_profile=foo", "--listen_address=host:1234"}, 2, incompatibleSettings},
		{"CpuAndMemAndListenAddress", []string{"--cpu_profile=foo", "--listen_address=host:1234", "--mem_profile=bar"}, 2, incompatibleSettings},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := utils.RunAndWait(d.wantExitCode, append(d.args, "/non-existent")...)
			if err != nil {
				t.Fatal(err)
			}
			if len(stdout) > 0 {
				t.Errorf("Got %s; want stdout to be empty", stdout)
			}
			if !utils.MatchesRegexp(d.wantStderr, stderr) {
				t.Errorf("Got %s; want stderr to match %s", stderr, d.wantStderr)
			}
			if d.wantExitCode == 2 {
				if !utils.MatchesRegexp("--help", stderr) {
					t.Errorf("Got %s; want --help mention in stderr", stderr)
				}
			}
		})
	}
}
