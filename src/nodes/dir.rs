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

use {Cache, IdGenerator};
use self::time::Timespec;
use std::collections::HashMap;
use std::ffi::{OsStr, OsString};
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use super::conv;
use super::KernelError;
use super::Node;
use super::NodeResult;

/// Representation of a directory node.
pub struct Dir {
    inode: u64,
    writable: bool,
    state: Mutex<MutableDir>,
}

/// Holds the mutable data of a directory node.
struct MutableDir {
    parent: u64,
    underlying_path: Option<PathBuf>,
    attr: fuse::FileAttr,
    children: HashMap<OsString, Arc<Node>>,
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

        #[cfg_attr(feature = "cargo-clippy", allow(redundant_field_names))]
        let state = MutableDir {
            parent: inode,
            underlying_path: None,
            attr: attr,
            children: HashMap::new(),
        };

        Arc::new(Dir {
            inode,
            writable: false,
            state: Mutex::from(state),
        })
    }

    /// Creates a new directory whose contents are backed by another directory.
    ///
    /// `inode` is the node number to assign to the created in-memory directory and has no relation
    /// to the underlying directory.  `underlying_path` indicates the path to the directory outside
    /// of the sandbox that backs this one.  `fs_attr` contains the stat data for the given path.
    ///
    /// `fs_attr` is an input parameter because, by the time we decide to instantiate a directory
    /// node (e.g. as we discover directory entries during readdir or lookup), we have already
    /// issued a stat on the underlying file system and we cannot re-do it for efficiency reasons.
    pub fn new_mapped(inode: u64, underlying_path: &Path, fs_attr: &fs::Metadata, writable: bool)
        -> Arc<Node> {
        if !fs_attr.is_dir() {
            panic!("Can only construct based on dirs");
        }
        let attr = conv::attr_fs_to_fuse(underlying_path, inode, &fs_attr);

        #[cfg_attr(feature = "cargo-clippy", allow(redundant_field_names))]
        let state = MutableDir {
            parent: inode,
            underlying_path: Some(PathBuf::from(underlying_path)),
            attr: attr,
            children: HashMap::new(),
        };

        Arc::new(Dir { inode, writable, state: Mutex::from(state) })
    }
}

impl Node for Dir {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();

        let check_type = |t: fs::FileType| if t.is_dir() { Ok(()) } else { Err("directory") };
        if let Some(attr) = super::get_new_attr(
            self.inode, state.underlying_path.as_ref(), check_type)? {
            state.attr = attr;
        }

        Ok(state.attr)
    }

    fn lookup(&self, name: &OsStr, ids: &IdGenerator, cache: &Cache)
        -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();

        if let Some(node) = state.children.get(name) {
            let refreshed_attr = node.getattr()?;
            return Ok((node.clone(), refreshed_attr))
        }

        let (child, attr) = {
            let path = match state.underlying_path.as_ref() {
                Some(underlying_path) => underlying_path.join(name),
                None => return Err(KernelError::from_errno(libc::ENOENT)),
            };
            let fs_attr = fs::symlink_metadata(&path)?;
            let node = cache.get_or_create(ids, &path, &fs_attr, self.writable);
            let attr = conv::attr_fs_to_fuse(path.as_path(), node.inode(), &fs_attr);
            (node, attr)
        };
        state.children.insert(name.to_os_string(), child.clone());
        Ok((child, attr))
    }

    fn readdir(&self, ids: &IdGenerator, cache: &Cache, reply: &mut fuse::ReplyDirectory)
        -> NodeResult<()> {
        let mut state = self.state.lock().unwrap();

        reply.add(self.inode, 0, fuse::FileType::Directory, ".");
        reply.add(state.parent, 1, fuse::FileType::Directory, "..");
        let mut pos = 2;

        // TODO(jmmv): Handle user-provided mappings before underlying entries.

        if state.underlying_path.as_ref().is_none() {
            return Ok(());
        }

        let entries = fs::read_dir(state.underlying_path.as_ref().unwrap())?;
        for entry in entries {
            let entry = entry?;
            let name = entry.file_name();
            let fs_attr = entry.metadata()?;

            let path = state.underlying_path.as_ref().unwrap().join(&name);
            let child = cache.get_or_create(ids, &path, &fs_attr, self.writable);

            reply.add(child.inode(), pos,
                conv::filetype_fs_to_fuse(&path, fs_attr.file_type()), &name);
            // Do the insertion into state.children after calling reply.add() to be able to move
            // the name into the key without having to copy it again.
            state.children.insert(name, child.clone());

            pos += 1;
        }
        Ok(())
    }
}
