// Copyright 2018 Google Inc.
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
	"flag"
	"log"
	"os"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

var (
	features         = flag.String("features", "", "Whitespace-separated list of features enabled during the build")
	releaseBuild     = flag.Bool("release_build", true, "Whether the tested binary was built for release or not")
	sandboxfsBinary  = flag.String("sandboxfs_binary", "", "Path to the sandboxfs binary to test; cannot be empty and must point to an existent binary")
	unprivilegedUser = flag.String("unprivileged_user", "", "Username of the system user to use for tests that require non-root permissions; can be empty, in which case those tests are skipped")
)

func TestMain(m *testing.M) {
	flag.Parse()
	if len(*sandboxfsBinary) == 0 {
		log.Fatalf("--sandboxfs_binary must be provided")
	}
	if err := utils.SetConfigFromFlags(*features, *releaseBuild, *sandboxfsBinary, *unprivilegedUser); err != nil {
		log.Fatalf("invalid flags configuration: %v", err)
	}

	os.Exit(m.Run())
}
