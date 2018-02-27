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
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

var (
	// versionPattern contains a pattern to match the output of sandboxfs --version.
	versionPattern = `sandboxfs [0-9]+\.[0-9]+`
)

func TestCli_Help(t *testing.T) {
	wantStdout := `Usage: sandboxfs [flags...] mount-point

Available flags:
  -allow value
    	specifies who should have access to the file system; must be one of other, root, or self (default self)
  -cpu_profile string
    	write a CPU profile to the given file on exit
  -debug
    	log details about FUSE requests and responses to stderr
  -help
    	print the usage information and exit
  -input string
    	where to read the configuration data from (- for stdin) (default "-")
  -listen_address string
    	enable HTTP server on the given address and expose pprof data
  -mapping value
    	mappings of the form TYPE:MAPPING:TARGET
  -mem_profile string
    	write a memory profile to the given file on exit
  -output string
    	where to write the status of reconfiguration to (- for stdout) (default "-")
  -version
    	show version information and exit
  -volume_name string
    	name for the sandboxfs volume (default "sandbox")
`

	stdout, stderr, err := utils.RunAndWait(0, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if wantStdout != stdout {
		t.Errorf("Got %s; want stdout to match %s", stdout, wantStdout)
	}
	if len(stderr) > 0 {
		t.Errorf("Got %s; want stderr to be empty", stderr)
	}
}
func TestCli_Version(t *testing.T) {
	stdout, stderr, err := utils.RunAndWait(0, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !utils.MatchesRegexp(versionPattern, stdout) {
		t.Errorf("Got %s; want stdout to match %s", stdout, versionPattern)
	}
	if len(stderr) > 0 {
		t.Errorf("Got %s; want stderr to be empty", stderr)
	}
}

func TestCli_VersionNotForRelease(t *testing.T) {
	if !utils.GetConfig().ReleaseBinary {
		t.Skipf("Binary intentionally built not for release")
	}

	stdout, _, err := utils.RunAndWait(0, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if utils.MatchesRegexp(`NOT.*FOR.*RELEASE`, stdout) {
		t.Errorf("Got %s; binary not built for release", stdout)
	}
}

func TestCli_ExclusiveFlagsPriority(t *testing.T) {
	testData := []struct {
		name string

		args           []string
		wantExitStatus int
		wantStdout     string
		wantStderr     string
	}{
		{"BogusFlagsWinOverEverything", []string{"--version", "--help", "--foo"}, 2, "", "not defined.*foo"},
		{"BogusHFlagWinsOverEverything", []string{"--version", "--help", "-h"}, 2, "", "not defined.*-h"},
		{"HelpWinsOverValidArgs", []string{"--version", "--mem_profile=foo", "--help", "--volume_name=foo"}, 0, "Usage:", ""},
		{"VersionWinsOverValidArgsButHelp", []string{"--mem_profile=foo", "--version", "--volume_name=foo"}, 0, versionPattern, ""},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := utils.RunAndWait(d.wantExitStatus, d.args...)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.wantStdout) == 0 && len(stdout) > 0 {
				t.Errorf("Got %s; want stdout to be empty", stdout)
			} else if len(d.wantStdout) > 0 && !utils.MatchesRegexp(d.wantStdout, stdout) {
				t.Errorf("Got %s; want stdout to match %s", stdout, d.wantStdout)
			}
			if len(d.wantStderr) == 0 && len(stderr) > 0 {
				t.Errorf("Got %s; want stderr to be empty", stderr)
			} else if len(d.wantStderr) > 0 && !utils.MatchesRegexp(d.wantStderr, stderr) {
				t.Errorf("Got %s; want stderr to match %s", stderr, d.wantStderr)
			}
		})
	}
}

func TestCli_Syntax(t *testing.T) {
	testData := []struct {
		name string

		args       []string
		wantStderr string
	}{
		{"InvalidFlag", []string{"--foo"}, "not defined.*-foo"},
		{"InvalidHFlag", []string{"-h"}, "not defined.*-h"},
		{"NoArguments", []string{}, "invalid number of arguments"},
		{"TooManyArguments", []string{"mount-point", "extra"}, "invalid number of arguments"},

		{"InvalidFlagWinsOverHelp", []string{"--invalid_flag", "--help"}, "not defined.*-invalid_flag"},
		// TODO(jmmv): For consistency with all previous tests, an invalid number of
		// arguments should win over --help, but it currently does not.  Fix or turn help
		// into a command of its own for consistency.
		// {"InvalidArgumentsWinOverHelp", []string{"--help", "foo"}, "number of arguments"},

		{"MappingMissingTarget", []string{"--mapping=ro:/foo"}, `invalid value "ro:/foo" for flag -mapping: flag "ro:/foo": expected contents to be of the form TYPE:MAPPING:TARGET`},
		{"MappingRelativeTarget", []string{"--mapping=rw:/:relative/path"}, `invalid value "rw:/:relative/path" for flag -mapping: path "relative/path": target must be an absolute path`},
		{"MappingBadType", []string{"--mapping=row:/foo:/bar"}, `invalid value "row:/foo:/bar" for flag -mapping: flag "row:/foo:/bar": unknown type row; must be one of ro,rw`},
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
