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
)

func TestCli_Help(t *testing.T) {
	generalHelp := `Usage: sandboxfs [flags...] subcommand ...
Subcommands:
  static   statically configured sandbox using command line flags.
  dynamic  dynamically configured sandbox using stdin.
Flags:
  -debug
    	log details about FUSE requests and responses to stderr
  -help
    	print the usage information and exit
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
	data := []struct {
		name string

		args       []string
		wantStdout string
	}{
		{"General", []string{"--help"}, generalHelp},
		{"Dynamic", []string{"dynamic", "--help"}, dynamicHelp},
		{"Static", []string{"static", "--help"}, staticHelp},
	}
	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := runAndWait(0, d.args...)
			if err != nil {
				t.Fatal(err)
			}
			if d.wantStdout != stdout {
				t.Errorf("got %s; want stdout to match %s", stdout, d.wantStdout)
			}
			if len(stderr) > 0 {
				t.Errorf("got %s; want stderr to be empty", stderr)
			}
		})
	}
}

func TestCli_Syntax(t *testing.T) {
	data := []struct {
		name string

		args       []string
		wantStderr string

		// TODO(jmmv): All invalid syntax errors (the ones validated in this test case)
		// should behave in the same manner: they should all show the same "help note" and
		// they should all return the same exit code.  Make them all consistent and remove
		// these test settings.
		wantExitStatus int
		wantHelpNote   bool
	}{
		{"InvalidFlag", []string{"--foo"}, "not defined.*-foo", 2, false},
		{"NoArguments", []string{}, "Invalid number of arguments", 1, true},
		{"InvalidCommand", []string{"foo"}, "Invalid command", 1, true},

		{"DynamicNoArguments", []string{"dynamic"}, "Invalid number of arguments", 1, true},
		{"DynamicInvalidFlag", []string{"dynamic", "--bar"}, "not defined.*-bar", 2, false},
		{"DynamicTooManyArguments", []string{"dynamic", "a", "b"}, "Invalid number of arguments", 1, true},

		{"StaticNoArguments", []string{"static"}, "Invalid number of arguments", 1, true},
		{"StaticInvalidFlag", []string{"static", "--baz"}, "not defined.*-baz", 2, false},
		{"StaticTooManyArguments", []string{"static", "a", "b"}, "Invalid number of arguments", 1, true},

		{"InvalidFlagWinsOverHelp", []string{"--invalid_flag", "--help"}, "not defined.*-invalid_flag", 2, false},
		{"InvalidCommandWinsOverHelp", []string{"foo", "--help"}, "Invalid command", 1, true},
		{"DynamicInvalidFlagWinsOverHelp", []string{"dynamic", "--invalid_flag", "--help"}, "not defined.*-invalid_flag", 2, false},
		{"StaticInvalidFlagWinsOverHelp", []string{"static", "--invalid_flag", "--help"}, "not defined.*-invalid_flag", 2, false},
		// TODO(jmmv): For consistency with all previous tests, an invalid number of
		// arguments should win over --help, but it currently does not.  Fix or turn help
		// into a command of its own for consistency.  {"InvalidArgumentsWinOverHelp",
		// []string{"--help", "foo"}, "number of arguments"},
	}
	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := runAndWait(d.wantExitStatus, d.args...)
			if err != nil {
				t.Fatal(err)
			}
			if len(stdout) > 0 {
				t.Errorf("got %s; want stdout to be empty", stdout)
			}
			if !matchesRegexp(d.wantStderr, stderr) {
				t.Errorf("got %s; want stderr to match %s", stderr, d.wantStderr)
			}
			if d.wantHelpNote {
				if !matchesRegexp("pass -help flag for details", stderr) {
					t.Errorf("no help note found in stderr")
				}
			} else {
				if matchesRegexp("pass -help flag for details", stderr) {
					t.Errorf("help note found in stderr")
				}
			}
		})
	}
}

func TestCli_StaticMappingsSyntax(t *testing.T) {
	data := []struct {
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
	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := runAndWait(2, "static", fmt.Sprintf("--%s=%s", d.flagName, d.flagValue), "irrelevant-mount-point")
			if err != nil {
				t.Fatal(err)
			}
			if len(stdout) > 0 {
				t.Errorf("got %s; want stdout to be empty", stdout)
			}
			if d.wantStderr != stderr {
				t.Errorf("got %s; want stderr to match %s", stderr, d.wantStderr)
			}
		})
	}
}
