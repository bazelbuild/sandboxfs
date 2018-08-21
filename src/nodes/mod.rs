// Copyright 2018 Google Inc.
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

use fuse;
use std::ffi::OsStr;
use std::sync::Arc;

mod dir;
pub use self::dir::Dir;

/// Type that represents an error understood by the kernel.
#[derive(Debug, Fail)]
#[fail(display = "errno={}", errno)]
pub struct KernelError {
    errno: i32,
}

impl KernelError {
    /// Constructs a new error given a raw errno code.
    fn from_errno(errno: i32) -> KernelError {
        KernelError { errno }
    }

    /// Obtains the errno code contained in this error, which can be fed back into the kernel.
    pub fn errno(&self) -> i32 {
        return self.errno;
    }
}

/// Generic result type for of all node operations.
pub type NodeResult<T> = Result<T, KernelError>;

/// Abstract representation of a file system node.
///
/// Due to the way nodes and node operations are represented in the kernel, this trait exposes a
/// collection of methods that do not all make sense for all possible node types: some methods will
/// only make sense for directories and others will only make sense for regular files, for example.
/// These conflicting methods come with a default implementation that panics.
pub trait Node {
    /// Returns the inode number of this node.
    ///
    /// The inode number is immutable and, as such, this information can be queried without having
    /// to lock the node.
    fn inode(&self) -> u64;

    /// Retrieves the node's metadata.
    fn getattr(&self) -> NodeResult<fuse::FileAttr>;

    /// Looks up a node with the given name within the current node and returns the found node and
    /// its attributes at the time of the query.
    ///
    /// The attributes are returned to avoid having to relock the node on the caller side in order
    /// to supply those attributes to the kernel.
    fn lookup(&self, _name: &OsStr) -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        panic!("Not implemented");
    }

    /// Reads all directory entries into the given reply object.
    ///
    /// It is the responsibility of the caller to invoke `reply.ok()` on the reply object.  This is
    /// for consistency with the handling of any errors returned by this function.
    fn readdir(&self, _reply: &mut fuse::ReplyDirectory) -> NodeResult<()> {
        panic!("Not implemented");
    }
}
