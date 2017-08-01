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

	"golang.org/x/net/context"
	"bazil.org/fuse/fs"
	"bazil.org/fuse"
)

// Symlink corresponds to symlinks in an in-memory representation of the
// filesystem tree.
type Symlink struct {
	BaseNode
}

// newSymlink initializes a new Symlink node with the proper inode number.
func newSymlink(path string, id DevInoPair) *Symlink {
	return &Symlink{newBaseNode(path, id)}
}

// Readlink reads a symlink and returns the string path to its destination.
func (s *Symlink) Readlink(_ context.Context, req *fuse.ReadlinkRequest) (string, error) {
	link, err := os.Readlink(s.underlyingPath)
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

// invalidateRecursively clears the kernel cache corresponding to this node,
// and children if present.
func (s *Symlink) invalidateRecursively(server *fs.Server) {
	err := server.InvalidateNodeData(s)
	logCacheInvalidationError(err, "Could not invalidate node cache: ", s)
}
