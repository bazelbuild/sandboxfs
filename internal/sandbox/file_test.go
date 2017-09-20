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

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

var OpenRequestFile = &fuse.OpenRequest{
	Dir:   false,
	Flags: fuse.OpenReadOnly,
}

func fileSetup(t *testing.T) string {
	src, err := ioutil.TempDir("", "test_src_")
	if err != nil {
		t.Fatal("Setup failed with error: ", err)
	}
	return src
}

func fileTeardown(src string, t *testing.T) {
	if err := os.RemoveAll(src); err != nil {
		t.Fatal("Teardown failed with error: ", err)
	}
}

func TestFile_Read(t *testing.T) {
	src := fileSetup(t)
	defer fileTeardown(src, t)
	contents := "1234567890"
	if err := ioutil.WriteFile(src+"/a", []byte(contents), 0644); err != nil {
		t.Fatal("File write failed with error: ", err)
	}

	for _, writable := range []bool{false, true} {
		d := newFile(src+"/a", DevInoPair{}, writable)
		h, err := d.Open(context.Background(), OpenRequestFile, nil)
		if err != nil {
			t.Error("File open failed with error: ", err)
		}

		readReq := &fuse.ReadRequest{
			Dir:    false,
			Size:   2,
			Offset: 3,
		}
		readResp := &fuse.ReadResponse{
			Data: make([]byte, readReq.Size),
		}
		if err := h.(*OpenFile).Read(context.Background(), readReq, readResp); err != nil {
			t.Error("Error reading file 'a':", err)
		}
		if got := string(readResp.Data); got != contents[3:5] {
			t.Errorf("File content incorrect: got %q, want %q", got, contents[3:5])
		}

		readReq = &fuse.ReadRequest{
			Dir:    false,
			Size:   4,
			Offset: 2,
		}
		readResp = &fuse.ReadResponse{
			Data: make([]byte, readReq.Size),
		}
		if err := h.(*OpenFile).Read(context.Background(), readReq, readResp); err != nil {
			t.Error("Error reading file 'a':", err)
		}
		if got := string(readResp.Data); got != contents[2:6] {
			t.Errorf("File content incorrect: got %q, want %q", got, contents[2:6])
		}

		if err := h.(fs.HandleReleaser).Release(context.Background(), nil); err != nil {
			t.Error("File close failed with error: ", err)
		}
	}
}

func TestFile_Write_Error(t *testing.T) {
	src := fileSetup(t)
	defer fileTeardown(src, t)

	content := "1234567890"
	if err := ioutil.WriteFile(src+"/a", []byte(content), 0644); err != nil {
		t.Fatal("File write failed with error: ", err)
	}
	f := newFile(src+"/a", DevInoPair{}, false)
	h, err := f.Open(context.Background(), &fuse.OpenRequest{Dir: false, Flags: fuse.OpenWriteOnly}, nil)
	if err != nil {
		t.Error("File open failed with error: ", err)
	}

	writeReq := &fuse.WriteRequest{
		Offset: int64(3),
		Data:   []byte("abcdefghi"),
	}
	writeResp := &fuse.WriteResponse{}

	if err := h.(*OpenFile).Write(context.Background(), writeReq, writeResp); err != fuseErrno(unix.EPERM) {
		t.Errorf("Read-only file write failed with error: %T(%v), want: fuse.Errno(unix.EPERM)", err, err)
	}
	if text, err := ioutil.ReadFile(src + "/a"); err != nil || string(text) != content {
		t.Errorf("ReadFile got (%q, %v), expected (%q, nil)", string(text), err, content)
	}
}

func TestFile_Write_Ok(t *testing.T) {
	src := fileSetup(t)
	defer fileTeardown(src, t)

	contents := "1234567890"
	written := "abcdefghi"
	offset := 3

	if err := ioutil.WriteFile(src+"/a", []byte(contents), 0644); err != nil {
		t.Fatal("File write failed with error: ", err)
	}
	f := newFile(src+"/a", DevInoPair{}, true)
	h, err := f.Open(context.Background(), &fuse.OpenRequest{Dir: false, Flags: fuse.OpenWriteOnly}, nil)
	if err != nil {
		t.Error("File open failed with error: ", err)
	}

	writeReq := &fuse.WriteRequest{
		Offset: int64(offset),
		Data:   []byte(written),
	}
	writeResp := &fuse.WriteResponse{}
	if err := h.(*OpenFile).Write(context.Background(), writeReq, writeResp); err != nil {
		t.Error("File write failed with error: ", err)
	}
	got, err := ioutil.ReadFile(src + "/a")
	if err != nil {
		t.Error("Reading from newly written file gave error: ", err)
	}
	if want := contents[:offset] + written; string(got) != want {
		t.Errorf("File content is: %q, want: %q", string(got), string(want))
	}
	if writeResp.Size != len(written) {
		t.Errorf("WriteResponse had written size: %v, want: %v", writeResp.Size, len(written))
	}
}
