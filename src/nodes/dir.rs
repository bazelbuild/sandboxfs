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

extern crate fuse;
extern crate libc;
extern crate time;

use std::ffi::OsStr;
use std::sync::{Arc, Mutex};
use self::time::Timespec;
use super::KernelError;
use super::Node;
use super::NodeResult;

/// Representation of a directory node.
pub struct Dir {
    inode: u64,
    state: Mutex<MutableDir>,
}

/// Holds the mutable data of a directory node.
struct MutableDir {
    parent: u64,
    attr: fuse::FileAttr,
}

impl Dir {
    /// Creates a new scaffold directory to represent the root directory.
    ///
    /// `time` is the timestamp to be used for all node times.
    ///
    /// `uid` and `gid` indicate the ownership details of the node.  These should always match the
    /// values of the currently-running process -- but not necessarily if we want to let users
    /// customize these via flags at some point.
    pub fn new_root(time: Timespec, uid: u32, gid: u32) -> Arc<Node> {
        let inode = fuse::FUSE_ROOT_ID;

        #[cfg_attr(feature = "cargo-clippy", allow(redundant_field_names))]
        let attr = fuse::FileAttr {
            ino: inode,
            kind: fuse::FileType::Directory,
            nlink: 2,  // "." entry plus whichever initial named node points at this.
            size: 2,  // TODO(jmmv): Reevaluate what directory sizes should be.
            blocks: 1,  // TODO(jmmv): Reevaluate what directory blocks should be.
            atime: time,
            mtime: time,
            ctime: time,
            crtime: time,
            perm: 0o555 as u16,  // Scaffold directories cannot be mutated by the user.
            uid: uid,
            gid: gid,
            rdev: 0,
            flags: 0,
        };

        let state = MutableDir { parent: inode, attr };

        Arc::new(Dir {
            inode,
            state: Mutex::from(state),
        })
    }
}

impl Node for Dir {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let state = self.state.lock().unwrap();
        Ok(state.attr)
    }

    fn lookup(&self, _name: &OsStr) -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        Err(KernelError::from_errno(libc::ENOENT))
    }

    fn readdir(&self, reply: &mut fuse::ReplyDirectory) -> NodeResult<()> {
        let state = self.state.lock().unwrap();

        reply.add(self.inode, 0, fuse::FileType::Directory, ".");
        reply.add(state.parent, 1, fuse::FileType::Directory, "..");
        Ok(())
    }
}
