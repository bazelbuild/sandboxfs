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

package sandbox

import (
	"io/ioutil"
	"os"
	"testing"

	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

func symlinkSetup(t *testing.T) string {
	src, err := ioutil.TempDir("", "test_src_")
	if err != nil {
		t.Fatal("Setup failed with error: ", err)
	}
	return src
}

func symlinkTeardown(src string, t *testing.T) {
	if err := os.RemoveAll(src); err != nil {
		t.Fatal("Teardown failed with error: ", err)
	}
}

func TestSymlink_Readlink(t *testing.T) {
	src := symlinkSetup(t)
	defer symlinkTeardown(src, t)
	if err := os.MkdirAll(src+"/B", 0755); err != nil {
		t.Fatal("Mkdir failed with error: ", err)
	}
	if err := os.Symlink(src+"/A/B/C/D/E/f", src+"/B/e"); err != nil {
		t.Fatal("Symlink failed with error: ", err)
	}

	d := newSymlink(src+"/B/e", DevInoPair{})
	path, err := d.Readlink(context.Background(), nil)
	if err != nil {
		t.Error("Readlink failed with error:", err)
	}
	if want := src + "/A/B/C/D/E/f"; path != want {
		t.Errorf("Readlink returned wrong path: %q, want: %q", path, want)
	}
}

func TestSymlink_Readlink_Error(t *testing.T) {
	src := symlinkSetup(t)
	defer symlinkTeardown(src, t)
	if err := os.MkdirAll(src+"/A/B", 0755); err != nil {
		t.Fatal("setup failed with error: ", err)
	}

	d := newSymlink(src+"/A/B", DevInoPair{})
	_, err := d.Readlink(context.Background(), nil)
	if err != fuseErrno(unix.EINVAL) {
		t.Errorf("Readlink returned error %v, expected %v:", err, fuseErrno(unix.EINVAL))
	}
}
