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

// Root represents the node at the root of the file system.
//
// The root node cannot be a Dir in itself because of reconfigurations: during a reconfiguration,
// we replace the whole directory tree with a new instance so we have to swap the pointer to the
// root directory.
//
// TODO(jmmv): Now that we have a single implementation for directory nodes, we can get rid of the
// Root indirection by folding reconfigurations into the directory itself.
type Root struct {
	// dir holds the directory backing the root node. Note that the FUSE API is unaware of this
	// backing node: any operations that reference the root node must do so through the Root
	// instance. In that sense, the directory here is just an implementation detail.
	dir *Dir

	// mu protects reads and updates to the dir pointer.
	mu sync.RWMutex
}

// NewRoot returns a new instance of Root with the appropriate underlying node.
func NewRoot(node *Dir) *Root {
	return &Root{dir: node}
}

// getDir returns the dir member atomically.
func (r *Root) getDir() *Dir {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dir
}

// Attr delegates the Attr operation to the backing directory node.
func (r *Root) Attr(ctx context.Context, a *fuse.Attr) error {
	return r.getDir().Attr(ctx, a)
}

// Create delegates the Create operation to the backing directory node.
func (r *Root) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	return r.getDir().Create(ctx, req, resp)
}

// Link creates a hard link.
func (r *Root) Link(ctx context.Context, req *fuse.LinkRequest, old fs.Node) (fs.Node, error) {
	return r.getDir().Link(ctx, req, old)
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

// Open always returns self, which represents a single handle for the root directory.
//
// We cannot delegate this operation to the backing directory because the backing directory changes
// during reconfigurations.  As a result, any open handles on those backing directories would become
// invalid across reconfigurations.  By keeping a single instance for the root, we can just delegate
// to the backing directories when needed.
func (r *Root) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	return r, nil
}

// ReadDirAll obtains the directory contents of the underlying directory type.  This is done by
// stringing a series of open/read/release requests on the backing directory, simulating what the
// kernel would do.
//
// TODO(jmmv): This is not semantically correct: we shouldn't be "opening" the backing directory as
// we do below, because a readdir operation from the kernel on an already-open root directory causes
// a spurious open of an unrelated entity.  We may be able to remove this once we fold
// reconfigurations into directories and thus preserve the identity of the root directory.
func (r *Root) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	handle, err := r.dir.Open(ctx, &fuse.OpenRequest{}, &fuse.OpenResponse{})
	if err != nil {
		return nil, err
	}
	if typedHandle, ok := handle.(fs.HandleReadDirAller); ok {
		dirents, err := typedHandle.ReadDirAll(ctx)
		if err != nil {
			return nil, err
		}

		if typedHandle, ok := handle.(fs.HandleReleaser); ok {
			if err := typedHandle.Release(ctx, nil); err != nil {
				return nil, err
			}
		}

		return dirents, nil
	}
	panic("Handles for backing directories are expected to implement ReadDirAll")
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
