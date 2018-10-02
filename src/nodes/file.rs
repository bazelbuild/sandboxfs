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
extern crate time;

use nix::errno;
use nodes::{conv, KernelError, Node, NodeResult};
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Representation of a file node.
///
/// File nodes represent all kinds of files (except for directories and symlinks), not just regular
/// files, because the set of node operations required by them is the same.
pub struct File {
    inode: u64,
    writable: bool,
    state: Mutex<MutableFile>,
}

/// Holds the mutable data of a file node.
struct MutableFile {
    underlying_path: PathBuf,
    attr: fuse::FileAttr,
}

impl File {
    /// Returns true if this node can represent the given file type.
    fn supports_type(t: fs::FileType) -> bool {
        !t.is_dir() && !t.is_symlink()
    }

    /// Creates a new file backed by a file on an underlying file system.
    ///
    /// `inode` is the node number to assign to the created in-memory file and has no relation
    /// to the underlying file.  `underlying_path` indicates the path to the file outside
    /// of the sandbox that backs this one.  `fs_attr` contains the stat data for the given path.
    ///
    /// `fs_attr` is an input parameter because, by the time we decide to instantiate a file
    /// node (e.g. as we discover directory entries during readdir or lookup), we have already
    /// issued a stat on the underlying file system and we cannot re-do it for efficiency reasons.
    pub fn new_mapped(inode: u64, underlying_path: &Path, fs_attr: &fs::Metadata, writable: bool)
        -> Arc<Node> {
        if !File::supports_type(fs_attr.file_type()) {
            panic!("Can only construct based on non-directories / non-symlinks");
        }
        let attr = conv::attr_fs_to_fuse(underlying_path, inode, &fs_attr);

        let state = MutableFile {
            underlying_path: PathBuf::from(underlying_path),
            attr,
        };

        Arc::new(File { inode, writable, state: Mutex::from(state) })
    }
}

impl Node for File {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();

        let fs_attr = fs::symlink_metadata(&state.underlying_path)?;
        if !File::supports_type(fs_attr.file_type()) {
            warn!("Path {:?} backing a file node is no longer a file; got {:?}",
                &state.underlying_path, fs_attr.file_type());
            return Err(KernelError::from_errno(errno::Errno::EIO));
        }
        state.attr = conv::attr_fs_to_fuse(&state.underlying_path, self.inode, &fs_attr);

        Ok(state.attr)
    }
}
