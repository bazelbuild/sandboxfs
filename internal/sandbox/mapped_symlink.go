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
	"os"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// MappedSymlink is a node that represents a symlink backed by another symlink that lives outside of
// the mount point.
type MappedSymlink struct {
	BaseNode
}

// newMappedSymlink initializes a new MappedSymlink node with the proper inode number.
func newMappedSymlink(path string, fileInfo os.FileInfo, writable bool) *MappedSymlink {
	return &MappedSymlink{
		BaseNode: newBaseNode(path, fileInfo, writable),
	}
}

// Readlink reads a symlink and returns the string path to its destination.
func (s *MappedSymlink) Readlink(_ context.Context, req *fuse.ReadlinkRequest) (string, error) {
	link, err := os.Readlink(s.underlyingPath)
	return link, fuseErrno(err)
}

// Dirent returns the directory entry corresponding to the symlink.
func (s *MappedSymlink) Dirent(name string) fuse.Dirent {
	return fuse.Dirent{
		Inode: s.Inode(),
		Name:  name,
		Type:  fuse.DT_Link,
	}
}

// invalidate clears the kernel cache corresponding to this symlink.
func (s *MappedSymlink) invalidate(server *fs.Server) {
	err := server.InvalidateNodeData(s)
	logCacheInvalidationError(err, "Could not invalidate node cache: ", s)
}
