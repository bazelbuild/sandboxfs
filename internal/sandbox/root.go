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
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// cacheInvalidator represents a node with kernel cache invalidation abilities.
type cacheInvalidator interface {
	fs.Node

	invalidate(*fs.Server)
}

// Dir defines the interfaces satisfied by all directory types.
type Dir interface {
	fs.Node
	fs.NodeCreater
	fs.NodeStringLookuper
	fs.NodeMkdirer
	fs.NodeMknoder
	fs.NodeOpener
	fs.NodeRemover
	fs.NodeRenamer
	fs.NodeSetattrer
	fs.NodeSymlinker

	invalidateEntries(*fs.Server, fs.Node)
}

// Root represents the node at the root of the file system.
//
// The root node cannot be a Dir in itself because of reconfigurations: during a reconfiguration,
// the contents and type of the root directory may change (e.g. from MappedDir to ScaffoldDir) but
// the identity of the node cannot change because the FUSE API does not permit replacing the root
// node with another.  As a result, we have to wrap the actual directory being used.
//
// It's unlikely for the FUSE API to ever allow replacing the root directory because of the
// difficulties in doing so and the very limited use cases of such feature.
type Root struct {
	// dir holds the directory backing the root node. Note that the FUSE API is unaware of this
	// backing node: any operations that reference the root node must do so through the Root
	// instance. In that sense, the directory here is just an implementation detail.
	dir Dir

	// mu protects reads and updates to the dir pointer.
	mu sync.RWMutex
}

// NewRoot returns a new instance of Root with the appropriate underlying node.
func NewRoot(node Dir) *Root {
	return &Root{dir: node}
}

// getDir returns the dir member atomically.
func (r *Root) getDir() Dir {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dir
}

// Reconfigure resets the filesystem tree to the tree pointed to by newDir.
//
// It is important to note that a reconfiguration operation cannot stop other ongoing operations nor
// it cannot put new operations on hold until reconfiguration has completed. Trying to do so is
// futile. First because by the time FUSE gets a request, it's too late already: the request is
// already in-progress by the kernel and must be fulfilled. And second because this can result in
// deadlocks: e.g. cache invalidations may end up calling us back to look up the entries to be
// invalidated!
//
// The way we deal with this is simply by swapping the full file system contents atomically by
// exchanging the root directory with a new one. Ongoing operations on the old tree will continue to
// run until they complete, at which point the nodes will be released and discarded. New operations
// will hit the new tree as soon as it is swapped, which is fine because the new tree is ready for
// serving from the get go. Because cache invalidations happen out of band, they may cause erratic
// behavior on the ongoing operations... but this behavior is intentionally unspecified by us
// because it's not deterministic.
//
// Well-behaved users should only reconfigure the file system when they know it's quiescent, and
// this is what we specify in the documentation.
func (r *Root) Reconfigure(server *fs.Server, newDir Dir) {
	r.mu.Lock()
	oldDir := r.dir
	r.dir = newDir
	r.mu.Unlock()

	err := server.InvalidateNodeData(r)
	logCacheInvalidationError(err, "Could not invalidate root: ", r)

	// Invalidate the cache of the entries that are present before reconfiguration. This
	// essentially gets rid of entries that will be no longer available.
	oldDir.invalidateEntries(server, r)

	// Invalidate the cache of entries that were previously returning ENOENT.
	newDir.invalidateEntries(server, r)
}

// Attr delegates the Attr operation to the backing directory node.
func (r *Root) Attr(ctx context.Context, a *fuse.Attr) error {
	return r.getDir().Attr(ctx, a)
}

// Create delegates the Create operation to the backing directory node.
func (r *Root) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	return r.getDir().Create(ctx, req, resp)
}

// Lookup delegates the Lookup operation to the backing directory node.
func (r *Root) Lookup(ctx context.Context, name string) (fs.Node, error) {
	return r.getDir().Lookup(ctx, name)
}

// Mkdir delegates the Mkdir operation to the backing directory node.
func (r *Root) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	return r.getDir().Mkdir(ctx, req)
}

// Mknod delegates the Mknod operation to the backing directory node.
func (r *Root) Mknod(ctx context.Context, req *fuse.MknodRequest) (fs.Node, error) {
	return r.getDir().Mknod(ctx, req)
}

// Open delegates the Open operation to the backing directory node.
func (r *Root) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	return r.getDir().Open(ctx, req, resp)
}

// Remove delegates the Remove operation to the backing directory node.
func (r *Root) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	return r.getDir().Remove(ctx, req)
}

// Rename delegates the Rename operation to the backing directory node.
func (r *Root) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	r.mu.Lock()
	// When renaming a file within the root directory, we must pass the backing directory to the
	// rename operation.
	if newDir == r {
		newDir = r.dir
	}
	dir := r.dir
	r.mu.Unlock()

	return dir.Rename(ctx, req, newDir)
}

// Setattr delegates the Setattr operation to the backing directory node.
func (r *Root) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	return r.getDir().Setattr(ctx, req, resp)
}

// Symlink delegates the Symlink operation to the backing directory node.
func (r *Root) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	return r.getDir().Symlink(ctx, req)
}
