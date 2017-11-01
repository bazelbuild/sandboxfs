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
	"fmt"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

func TestCli_Help(t *testing.T) {
	generalHelp := `Usage: sandboxfs [flags...] subcommand ...
Subcommands:
  static   statically configured sandbox using command line flags.
  dynamic  dynamically configured sandbox using stdin.
Flags:
  -allow value
    	specifies who should have access to the file system; must be one of other, root, or self (default self)
  -cpu_profile string
    	write a CPU profile to the given file on exit
  -debug
    	log details about FUSE requests and responses to stderr
  -help
    	print the usage information and exit
  -listen_address string
    	enable HTTP server on the given address and expose pprof data
  -mem_profile string
    	write a memory profile to the given file on exit
  -volume_name string
    	name for the sandboxfs volume (default "sandbox")
`

	// TODO(jmmv): This help message should have the same structure as the general one.  E.g. it
	// should include details about general flags and the "Usage:" line should match the
	// structure of the general one.
	dynamicHelp := `Usage: sandboxfs dynamic MOUNT-POINT
  -help
    	print the usage information and exit
  -input string
    	where to read the configuration data from (- for stdin) (default "-")
  -output string
    	where to write the status of reconfiguration to (- for stdout) (default "-")
`

	// TODO(jmmv): This help message should have the same structure as the general one.  E.g. it
	// should include details about general flags and the "Usage:" line should match the
	// structure of the general one.
	staticHelp := `Usage: sandboxfs static [flags...] MOUNT-POINT
  -help
    	print the usage information and exit
  -read_only_mapping value
    	read-only mapping of the form MAPPING:TARGET
  -read_write_mapping value
    	read/write mapping of the form MAPPING:TARGET
`

	testData := []struct {
		name string

		args       []string
		wantStdout string
	}{
		{"General", []string{"--help"}, generalHelp},
		{"Dynamic", []string{"dynamic", "--help"}, dynamicHelp},
		{"Static", []string{"static", "--help"}, staticHelp},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := utils.RunAndWait(0, d.args...)
			if err != nil {
				t.Fatal(err)
			}
			if d.wantStdout != stdout {
				t.Errorf("Got %s; want stdout to match %s", stdout, d.wantStdout)
			}
			if len(stderr) > 0 {
				t.Errorf("Got %s; want stderr to be empty", stderr)
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
		{"NoArguments", []string{}, "invalid number of arguments"},
		{"InvalidCommand", []string{"foo"}, "invalid command"},

		{"DynamicNoArguments", []string{"dynamic"}, "invalid number of arguments"},
		{"DynamicInvalidFlag", []string{"dynamic", "--bar"}, "not defined.*-bar"},
		{"DynamicTooManyArguments", []string{"dynamic", "a", "b"}, "invalid number of arguments"},

		{"StaticNoArguments", []string{"static"}, "invalid number of arguments"},
		{"StaticInvalidFlag", []string{"static", "--baz"}, "not defined.*-baz"},
		{"StaticTooManyArguments", []string{"static", "a", "b"}, "invalid number of arguments"},

		{"InvalidFlagWinsOverHelp", []string{"--invalid_flag", "--help"}, "not defined.*-invalid_flag"},
		{"InvalidCommandWinsOverHelp", []string{"foo", "--help"}, "invalid command"},
		{"DynamicInvalidFlagWinsOverHelp", []string{"dynamic", "--invalid_flag", "--help"}, "not defined.*-invalid_flag"},
		{"StaticInvalidFlagWinsOverHelp", []string{"static", "--invalid_flag", "--help"}, "not defined.*-invalid_flag"},
		// TODO(jmmv): For consistency with all previous tests, an invalid number of
		// arguments should win over --help, but it currently does not.  Fix or turn help
		// into a command of its own for consistency.
		// {"InvalidArgumentsWinOverHelp", []string{"--help", "foo"}, "number of arguments"},
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

func TestCli_StaticMappingsSyntax(t *testing.T) {
	testData := []struct {
		name string

		flagName   string
		flagValue  string
		wantStderr string
	}{
		{"MissingTargetRO", "read_only_mapping", "/foo", `invalid value "/foo" for flag -read_only_mapping: flag "/foo": expected contents to be of the form MAPPING:TARGET` + "\n"},
		{"MissingTargetRW", "read_write_mapping", "/b", `invalid value "/b" for flag -read_write_mapping: flag "/b": expected contents to be of the form MAPPING:TARGET` + "\n"},

		{"RelativeTargetRO", "read_only_mapping", "/:relative/path", `invalid value "/:relative/path" for flag -read_only_mapping: path "relative/path": target must be an absolute path` + "\n"},
		{"RelativeTargetRW", "read_write_mapping", "/:other", `invalid value "/:other" for flag -read_write_mapping: path "other": target must be an absolute path` + "\n"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := utils.RunAndWait(2, "static", fmt.Sprintf("--%s=%s", d.flagName, d.flagValue), "irrelevant-mount-point")
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
