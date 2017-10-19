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
	"io"
	"os"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// MappedFile is a node that represents a file backed by another file that lives outside of the
// mount point.
type MappedFile struct {
	BaseNode
}

// openMappedFile is a handle returned when a file is opened.
type openMappedFile struct {
	nativeFile *os.File
	file       *MappedFile
}

var _ fs.Handle = (*openMappedFile)(nil)

// newMappedFile initializes a new MappedFile node with the proper inode number.
func newMappedFile(path string, id DevInoPair, writable bool) *MappedFile {
	return &MappedFile{
		BaseNode: newBaseNode(path, id, writable),
	}
}

// Open opens the file/directory in the underlying filesystem and returns a
// handle to it.
func (f *MappedFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	openedFile, err := os.OpenFile(f.underlyingPath, int(req.Flags), 0)
	if err != nil {
		return nil, fuseErrno(err)
	}
	return &openMappedFile{openedFile, f}, nil
}

// Setattr updates the file metadata and is also used to communicate file size changes.
func (f *MappedFile) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	ok, finalError := f.BaseNode.Setattr(ctx, req)
	if !ok {
		return finalError
	}

	if req.Valid.Size() {
		if err := os.Truncate(f.underlyingPath, int64(req.Size)); err != nil {
			if finalError == nil {
				finalError = err
			}
		}
	}

	return fuseErrno(finalError)
}

// Dirent returns the directory entry corresponding to the file.
func (f *MappedFile) Dirent(name string) fuse.Dirent {
	return fuse.Dirent{
		Inode: f.Inode(),
		Name:  name,
		Type:  fuse.DT_File,
	}
}

// Read sends the read requests to the corresponding file in the underlying
// filesystem.
func (o *openMappedFile) Read(_ context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	resp.Data = resp.Data[:req.Size]
	readBytes, err := o.nativeFile.ReadAt(resp.Data, req.Offset)
	if err != nil && err != io.EOF {
		return fuseErrno(err)
	}

	resp.Data = resp.Data[:readBytes]
	return nil
}

// Write sends the write requests to the corresponding file in the underlying
// filesystem.
func (o *openMappedFile) Write(_ context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if err := o.file.WantToWrite(); err != nil {
		return err
	}

	n, err := o.nativeFile.WriteAt(req.Data, req.Offset)
	resp.Size = n
	return fuseErrno(err)
}

// Fsync flushes the written contents to the backing file.
func (f *MappedFile) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	// Fsync is an operation that should be exposed on a fs.Handle, not
	// on a fs.Node, given that the fsync(2) system call operates on file
	// descriptors. There actually is a TODO in the FUSE implementation to
	// do this.
	//
	// TODO(pallavag): Even if the upstream API is wrong, we could manually
	// track open file descriptors and use any of them to invoke the
	// fsync(2). It's probably easier to improve the upstream API instead
	// of doing this here, hence why we haven't implemented this function.
	return nil
}

// Release closes the underlying file/directory handle.
func (o *openMappedFile) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	return fuseErrno(o.nativeFile.Close())
}

// invalidateRecursively clears the kernel cache corresponding to this node,
// and children if present.
func (f *MappedFile) invalidateRecursively(server *fs.Server) {
	err := server.InvalidateNodeData(f)
	logCacheInvalidationError(err, "Could not invalidate node cache: ", f)
}
