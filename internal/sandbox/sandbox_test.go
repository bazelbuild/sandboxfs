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
	"bufio"
	"io/ioutil"
	"os"
	"reflect"
	"strings"
	"testing"

	"bazil.org/fuse"
	"golang.org/x/sys/unix"
)

func setup(t *testing.T) string {
	src, err := ioutil.TempDir("", "test_src_")
	if err != nil {
		t.Fatal("setup failed with error: ", err)
	}
	return src
}

func teardown(src string, t *testing.T) {
	if err := os.RemoveAll(src); err != nil {
		t.Fatal("teardown failed with error: ", err)
	}
}

func TestSplitPath_Dir(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{".", []string{""}},
		{"./.", []string{""}},
		{"", []string{""}},
		{"/", []string{""}},
		{"/first/second/Third/FOURTH", []string{"", "first", "second", "Third", "FOURTH"}},
		{"/first/second/Third/FOURTH/", []string{"", "first", "second", "Third", "FOURTH"}},
		{"/first/second/../Third//////FOURTH", []string{"", "first", "Third", "FOURTH"}},
	}
	for _, test := range tests {
		if got := splitPath(test.path); !reflect.DeepEqual(got, test.want) {
			t.Errorf("splitPath(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestSandbox_TimespecToTime(t *testing.T) {
	testData := []struct {
		sec     int64
		nanosec int64
	}{
		{0, 0},
		{0, 100},
		{100, 0},
		{1000, 1000},
	}
	for _, data := range testData {
		ts := unix.Timespec{Sec: data.sec, Nsec: data.nanosec}
		if time := timespecToTime(ts); time.Unix() != data.sec || time.Nanosecond() != int(data.nanosec) {
			t.Errorf("timespecToTime failed: got (%v, %v), want (%v, %v)", time.Unix(), time.Nanosecond(), data.sec, data.nanosec)
		}
	}
}

func TestSandbox_FuseErrno(t *testing.T) {
	err := os.Chmod("/non_existent_dir", 0644)
	if err == nil {
		t.Error("Chmod passed, even though error was expected")
	}
	err = fuseErrno(err)
	if e, ok := err.(fuse.Errno); !ok || e != fuse.Errno(unix.ENOENT) {
		t.Errorf("Returned error was %T(%v), want fuse.Errno(unix.ENOENT)", err, err)
	}

	_, err = os.Readlink("/")
	if err == nil {
		t.Error("Readlink passed, even though error was expected")
	}
	if _, ok := fuseErrno(err).(fuse.Errno); !ok {
		t.Errorf("Returned error was of type %T, want fuse.Errno", fuseErrno(err))
	}

	err = unix.Chdir("")
	if err == nil {
		t.Error("Chdir passed, even though error was expected")
	}
	if _, ok := fuseErrno(err).(fuse.Errno); !ok {
		t.Errorf("Returned error was of type %T, want fuse.Errno", fuseErrno(err))
	}

	if got := fuseErrno(nil); got != nil {
		t.Errorf("fuseErrno(nil) returned: %v, want: nil", got)
	}
}

func TestSandbox_Init(t *testing.T) {
	src := setup(t)
	defer teardown(src, t)
	if err := ioutil.WriteFile(src+"/a", []byte(""), 0644); err != nil {
		t.Fatal("setup failed with error: ", err)
	}

	_, err := Init([]MappingSpec{{Mapping: "/", Target: src, Writable: true}})
	if err != nil {
		t.Error("Init failed with error: ", err)
	}
}

func TestSandbox_ReadConfig(t *testing.T) {
	input := `Text in line 1
Line 2 before empty line

Line 3 not part of chunk 1
Line 4 not part of chunk 1

`

	reader := bufio.NewReader(strings.NewReader(input))
	read, err := readConfig(reader)

	expected := `Text in line 1
Line 2 before empty line

`
	if err != nil || string(read) != expected {
		t.Errorf("Expected to read value (%q, nil), got (%q, %v)", expected, string(read), err)
	}

	read, err = readConfig(reader)

	expected = `Line 3 not part of chunk 1
Line 4 not part of chunk 1

`
	if err != nil || string(read) != expected {
		t.Errorf("Expected to read value (%q, nil), got (%q, %v)", expected, string(read), err)
	}
}
