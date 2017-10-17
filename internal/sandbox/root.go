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

// TODO(pallavag): Root data type is necessary since we want to reconfigure the
// node present at the root. However, FUSE API caches the value obtained from
// filesystem.Root() and does not allow us to change it.
// If the FUSE API allowed changing root and reserving, this file wouldn't be
// needed.

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

type cacheInvalidator interface {
	fs.Node
	invalidateRecursively(*fs.Server)
}

// dirCommon defines the interfaces satisfied by all directory types.
type dirCommon interface {
	fs.Node
	fs.NodeCreater
	fs.NodeStringLookuper
	fs.NodeMkdirer
	fs.NodeMknoder
	fs.NodeOpener
	fs.NodeRemover
	fs.NodeRenamer
	fs.NodeSymlinker

	invalidateRecursively(*fs.Server)
	invalidateRecursivelyParent(fs.Node, *fs.Server)
}

// Root is the container for all types that are valid to be at the root level
// of a filesystem tree.
type Root struct {
	dirCommon
}

// NewRoot returns a new instance of Root with the appropriate underlying node.
func NewRoot(node dirCommon) *Root {
	return &Root{node}
}

// Reconfigure resets the filesystem tree to the tree pointed to by root.
func (r *Root) Reconfigure(server *fs.Server, root dirCommon) {
	// NOTE: Double invalidation is done because we want to invalidate cache of 2 things.
	// 1. The things that were present in the previous tree, and are no longer available.
	// 2. The things that were returning ENOENT previously, but are available now.
	// The first invalidation is recursively called on the old tree. The second
	// invalidation is called on the tree when it has been updated.
	r.invalidateRecursively(server)
	r.dirCommon = root
	r.invalidateRecursively(server)
}

// invalidateRecursively clears kernel cache of itself, the embedded node and all
// its children recursively.
func (r *Root) invalidateRecursively(server *fs.Server) {
	err := server.InvalidateNodeData(r)
	logCacheInvalidationError(err, "Could not invalidate node cache: ", r)
	r.dirCommon.invalidateRecursivelyParent(r, server)
	r.dirCommon.invalidateRecursively(server)
}

// Rename delegates the Rename operation to the underlying node.
// NOTE: When renaming a file within the same directory, in root, we want
// newDir passed to be the underlying type and not the *Root type..
func (r *Root) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	if newDir == r {
		newDir = r.dirCommon
	}
	return r.dirCommon.Rename(ctx, req, newDir)
}
