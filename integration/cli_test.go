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
	"runtime"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

var (
	// versionPattern contains a pattern to match the output of sandboxfs --version.
	versionPattern = `sandboxfs [0-9]+\.[0-9]+`
)

func TestCli_Help(t *testing.T) {
	wantStdout := fmt.Sprintf(`Usage: sandboxfs [options] MOUNT_POINT

Options:
    --allow other|root|self
                        specifies who should have access to the file system
                        (default: self)
    --cpu_profile PATH  enables CPU profiling and writes a profile to the
                        given path
    --help              prints usage information and exits
    --input PATH        where to read reconfiguration data from (- for stdin)
    --mapping TYPE:PATH:UNDERLYING_PATH
                        type and locations of a mapping
    --node_cache        enables the path-based node cache (known broken)
    --output PATH       where to write the reconfiguration status to (- for
                        stdout)
    --reconfig_threads COUNT
                        number of reconfiguration threads (default: %d)
    --ttl TIMEs         how long the kernel is allowed to keep file metadata
                        (default: 60s)
    --version           prints version information and exits
    --xattrs            enables support for extended attributes
`, runtime.NumCPU())

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

func TestCli_ExclusiveFlagsPriority(t *testing.T) {
	testData := []struct {
		name string

		args           []string
		wantExitStatus int
		wantStdout     string
		wantStderr     string
	}{
		{
			"BogusFlagsWinOverEverything",
			[]string{"--version", "--help", "--foo"},
			2,
			"",
			"Unrecognized option.*'foo'",
		},
		{
			"BogusHFlagWinsOverEverything",
			[]string{"--version", "--help", "-h"},
			2,
			"",
			"Unrecognized option.*'h'",
		},
		{
			"HelpWinsOverValidArgs",
			[]string{"--version", "--allow=self", "--help", "/mnt"},
			0,
			"Usage:",
			"",
		},
		{
			"VersionWinsOverValidArgsButHelp",
			[]string{"--allow=other", "--version", "/mnt"},
			0,
			versionPattern,
			"",
		},
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
		{
			"InvalidFlag",
			[]string{"--foo"},
			"Unrecognized option.*'foo'",
		},
		{
			"InvalidHFlag",
			[]string{"-h"},
			"Unrecognized option.*'h'",
		},
		{
			"NoArguments",
			[]string{},
			"invalid number of arguments",
		},
		{
			"TooManyArguments",
			[]string{"mount-point", "extra"},
			"invalid number of arguments",
		},
		{
			"InvalidFlagWinsOverHelp",
			[]string{"--invalid_flag", "--help"},
			"Unrecognized option.*'invalid_flag'",
		},
		// TODO(jmmv): For consistency with all previous tests, an invalid number of
		// arguments should win over --help, but it currently does not.
		// {
		// 	"InvalidArgumentsWinOverHelp",
		// 	[]string{"--help", "foo"},
		// 	"invalid number of arguments",
		// },
		{
			"MappingMissingTarget",
			[]string{"--mapping=ro:/foo"},
			`bad mapping ro:/foo: expected three colon-separated fields`,
		},
		{
			"MappingRelativeTarget",
			[]string{"--mapping=rw:/:relative/path"},
			`bad mapping rw:/:relative/path: path "relative/path" is not absolute`,
		},
		{
			"MappingBadType",
			[]string{"--mapping=row:/foo:/bar"},
			`bad mapping row:/foo:/bar: type was row but should be ro or rw`,
		},
		{
			"ReconfigThreadsBadValue",
			[]string{"--reconfig_threads=-1"},
			`invalid thread count -1: .*invalid digit`,
		},
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
