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

// Symlink is a node that represents a symlink backed by another symlink that lives outside of
// the mount point.
type Symlink struct {
	BaseNode
}

// newSymlink creates a new symlink node to represent the given underlying path.
//
// This function should never be called to explicitly create nodes. Instead, use the getOrCreateNode
// function, which respects the global node cache.
func newSymlink(path string, fileInfo os.FileInfo, writable bool) *Symlink {
	return &Symlink{
		BaseNode: newBaseNode(path, fileInfo, writable),
	}
}

// Readlink reads a symlink and returns the string path to its destination.
func (s *Symlink) Readlink(_ context.Context, req *fuse.ReadlinkRequest) (string, error) {
	underlyingPath, isMapped := s.UnderlyingPath()
	if !isMapped {
		panic("Want to read a symlink but we don't have an underlying path")
	}

	link, err := os.Readlink(underlyingPath)
	return link, fuseErrno(err)
}

// Dirent returns the directory entry corresponding to the symlink.
func (s *Symlink) Dirent(name string) fuse.Dirent {
	return fuse.Dirent{
		Inode: s.Inode(),
		Name:  name,
		Type:  fuse.DT_Link,
	}
}

// invalidate clears the kernel cache corresponding to this symlink.
func (s *Symlink) invalidate(server *fs.Server) {
	// We assume that, as long as a Symlink object is alive, the node corresponds to a
	// non-deleted underlying symlink. Therefore, do not invalidate the node itself. This is
	// important to keep entries alive across reconfigurations, which helps performance.
}
