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
	"path/filepath"
	"testing"
)

func TestDebug_FuseOpsInLog(t *testing.T) {
	stderr := new(bytes.Buffer)

	state := mountSetupWithOutputs(t, nil, stderr, "--debug", "static", "-read_only_mapping=/:%ROOT%")
	defer state.tearDown(t)

	writeFileOrFatal(t, filepath.Join(state.root, "cookie"), 0644, "")

	if !matchesRegexp("Lookup.*cookie", stderr.String()) {
		t.Errorf("FUSE operations not found in stderr; debug flag did not reach FUSE library")
	}
}
