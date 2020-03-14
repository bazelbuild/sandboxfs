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

use failure::Fallible;
use nix::errno;
use nodes::{ArcNode, AttrDelta, Cache, KernelError, Node, NodeResult, conv, setattr};
use std::ffi::OsStr;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Representation of a symlink node.
pub struct Symlink {
    inode: u64,
    writable: bool,
    state: Mutex<MutableSymlink>,
}

/// Holds the mutable data of a symlink node.
struct MutableSymlink {
    underlying_path: Option<PathBuf>,
    attr: fuse::FileAttr,
}

impl Symlink {
    /// Creates a new symlink backed by a symlink on an underlying file system.
    ///
    /// `inode` is the node number to assign to the created in-memory symlink and has no relation
    /// to the underlying symlink.  `underlying_path` indicates the path to the symlink outside
    /// of the sandbox that backs this one.  `fs_attr` contains the stat data for the given path.
    ///
    /// `fs_attr` is an input parameter because, by the time we decide to instantiate a symlink
    /// node (e.g. as we discover directory entries during readdir or lookup), we have already
    /// issued a stat on the underlying file system and we cannot re-do it for efficiency reasons.
    pub fn new_mapped(inode: u64, underlying_path: &Path, fs_attr: &fs::Metadata, writable: bool)
        -> ArcNode {
        if !fs_attr.file_type().is_symlink() {
            panic!("Can only construct based on symlinks");
        }
        let attr = conv::attr_fs_to_fuse(underlying_path, inode, 1, &fs_attr);

        let state = MutableSymlink {
            underlying_path: Some(PathBuf::from(underlying_path)),
            attr: attr,
        };

        Arc::new(Symlink { inode, writable, state: Mutex::from(state) })
    }

    /// Same as `getattr` but with the node already locked.
    fn getattr_locked(inode: u64, state: &mut MutableSymlink) -> NodeResult<fuse::FileAttr> {
        if let Some(path) = &state.underlying_path {
            let fs_attr = fs::symlink_metadata(path)?;
            if !fs_attr.file_type().is_symlink() {
                warn!("Path {} backing a symlink node is no longer a symlink; got {:?}",
                    path.display(), fs_attr.file_type());
                return Err(KernelError::from_errno(errno::Errno::EIO));
            }
            state.attr = conv::attr_fs_to_fuse(path, inode, state.attr.nlink, &fs_attr);
        }

        Ok(state.attr)
    }
}

impl Node for Symlink {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn file_type_cached(&self) -> fuse::FileType {
        fuse::FileType::Symlink
    }

    fn delete(&self, cache: &dyn Cache) {
        let mut state = self.state.lock().unwrap();
        assert!(
            state.underlying_path.is_some(),
            "Delete already called or trying to delete an explicit mapping");
        cache.delete(state.underlying_path.as_ref().unwrap(), state.attr.kind);
        state.underlying_path = None;
    }

    fn set_underlying_path(&self, path: &Path, cache: &dyn Cache) {
        let mut state = self.state.lock().unwrap();
        debug_assert!(state.underlying_path.is_some(),
            "Renames should not have been allowed in scaffold or deleted nodes");
        cache.rename(
            state.underlying_path.as_ref().unwrap(), path.to_owned(), state.attr.kind);
        state.underlying_path = Some(PathBuf::from(path));
        debug_assert!(state.attr.nlink >= 1);
        state.attr.nlink -= 1;
    }

    fn unmap(&self, inodes: &mut Vec<u64>) -> Fallible<()> {
        inodes.push(self.inode);
        Ok(())
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        Symlink::getattr_locked(self.inode, &mut state)
    }

    fn getxattr(&self, name: &OsStr) -> NodeResult<Option<Vec<u8>>> {
        let state = self.state.lock().unwrap();
        assert!(
            state.underlying_path.is_some(),
            "There is no known API to access the extended attributes of a symlink via an fd");
        let value = xattr::get(state.underlying_path.as_ref().unwrap(), name)?;
        Ok(value)
    }

    fn listxattr(&self) -> NodeResult<Option<xattr::XAttrs>> {
        let state = self.state.lock().unwrap();
        assert!(
            state.underlying_path.is_some(),
            "There is no known API to access the extended attributes of a symlink via an fd");
        let xattrs = xattr::list(state.underlying_path.as_ref().unwrap())?;
        Ok(Some(xattrs))
    }

    fn readlink(&self) -> NodeResult<PathBuf> {
        let state = self.state.lock().unwrap();

        let path = state.underlying_path.as_ref().expect(
            "There is no known API to get the target of a deleted symlink");
        Ok(fs::read_link(path)?)
    }

    fn removexattr(&self, name: &OsStr) -> NodeResult<()> {
        let state = self.state.lock().unwrap();
        assert!(
            state.underlying_path.is_some(),
            "There is no known API to access the extended attributes of a symlink via an fd");
        xattr::remove(state.underlying_path.as_ref().unwrap(), name)?;
        Ok(())
    }

    fn setattr(&self, delta: &AttrDelta) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        state.attr = setattr(state.underlying_path.as_ref(), &state.attr, delta)?;
        Ok(state.attr)
    }

    fn setxattr(&self, name: &OsStr, value: &[u8]) -> NodeResult<()> {
        let state = self.state.lock().unwrap();
        assert!(
            state.underlying_path.is_some(),
            "There is no known API to access the extended attributes of a symlink via an fd");
        xattr::set(state.underlying_path.as_ref().unwrap(), name, value)?;
        Ok(())
    }
}
