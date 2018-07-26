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
use std::io;
use std::sync::{Arc, Mutex};
use self::time::Timespec;
use super::Node;

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
    /// Creates a new scaffold directory identified by the given `inode`.
    ///
    /// `parent` specifies the inode number of the parent directory which may be the same as
    /// `inode` when this directory represents the root of the file system.
    ///
    /// `time` is the timestamp to be used for all node times.
    ///
    /// `uid` and `gid` indicate the ownership details of the node.  For scaffold directories, these
    /// should always match the values of the currently-running process -- but not necessarily if we
    /// want to let users customize these via flags at some point.
    ///
    /// Scaffold directories are in-memory directories not backed by any on-disk contents.  These
    /// are used to represent user-specified mappings
    pub fn new_empty(inode: u64, parent: u64, time: Timespec, uid: u32, gid: u32) -> Arc<Node> {
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

        let state = MutableDir { parent, attr };

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

    fn getattr(&self) -> io::Result<fuse::FileAttr> {
        let state = self.state.lock().unwrap();
        Ok(state.attr.clone())
    }

    fn lookup(&self, _name: &OsStr) -> io::Result<(Arc<Node>, fuse::FileAttr)> {
        return Err(io::Error::from_raw_os_error(libc::ENOENT));
    }

    fn readdir(&self, reply: &mut fuse::ReplyDirectory) -> io::Result<()> {
        let state = self.state.lock().unwrap();

        reply.add(self.inode, 0, fuse::FileType::Directory, ".");
        reply.add(state.parent, 1, fuse::FileType::Directory, "..");
        Ok(())
    }
}
