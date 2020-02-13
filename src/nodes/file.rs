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

use Cache;
use nix::errno;
use nodes::{ArcHandle, ArcNode, AttrDelta, Handle, KernelError, Node, NodeResult, conv, setattr};
use std::ffi::OsStr;
use std::fs;
use std::os::unix::fs::FileExt;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Handle for an open file.
struct OpenFile {
    /// Reference to the node's state for this file.  Needed to update attributes on writes.
    state: Arc<Mutex<MutableFile>>,

    /// Handle for the open file descriptor.
    file: fs::File,
}

impl OpenFile {
    /// Creates a new handle that references the given node's `state` and the already-open `file`.
    fn from(state: Arc<Mutex<MutableFile>>, file: fs::File) -> OpenFile {
        Self { state, file }
    }
}

impl Handle for OpenFile {
    fn read(&self, offset: i64, size: u32) -> NodeResult<Vec<u8>> {
        let mut buffer = vec![0; size as usize];
        let n = self.file.read_at(&mut buffer[..size as usize], offset as u64)?;
        buffer.truncate(n);
        Ok(buffer)
    }

    fn write(&self, offset: i64, mut data: &[u8]) -> NodeResult<u32> {
        const MAX_WRITE: usize = std::u32::MAX as usize;
        if data.len() > MAX_WRITE {
            // We only do this check because FUSE wants an u32 as the return value but data could
            // theoretically be bigger.
            // TODO(jmmv): Should fix the FUSE libraries to just expose Rust API-friendly quantities
            // (usize in this case) and handle the kernel/Rust boundary internally.
            warn!("Truncating too-long write to {} (asked for {} bytes)", MAX_WRITE, data.len());
            data = &data[..MAX_WRITE];
        }

        let mut state = self.state.lock().unwrap();

        let n = self.file.write_at(data, offset as u64)?;
        debug_assert!(n <= MAX_WRITE, "Size bounds checked above");

        let new_size = (offset as u64) + (n as u64);
        if state.attr.size < new_size {
            state.attr.size = new_size;
        }

        Ok(n as u32)
    }
}

/// Representation of a file node.
///
/// File nodes represent all kinds of files (except for directories and symlinks), not just regular
/// files, because the set of node operations required by them is the same.
pub struct File {
    inode: u64,
    writable: bool,
    state: Arc<Mutex<MutableFile>>,
}

/// Holds the mutable data of a file node.
struct MutableFile {
    underlying_path: Option<PathBuf>,
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
        -> ArcNode {
        if !File::supports_type(fs_attr.file_type()) {
            panic!("Can only construct based on non-directories / non-symlinks");
        }
        let attr = conv::attr_fs_to_fuse(underlying_path, inode, 1, &fs_attr);

        let state = MutableFile {
            underlying_path: Some(PathBuf::from(underlying_path)),
            attr: attr,
        };

        Arc::new(File { inode, writable, state: Arc::from(Mutex::from(state)) })
    }

    /// Same as `getattr` but with the node already locked.
    fn getattr_locked(inode: u64, state: &mut MutableFile) -> NodeResult<fuse::FileAttr> {
        if let Some(path) = &state.underlying_path {
            let fs_attr = fs::symlink_metadata(path)?;
            if !File::supports_type(fs_attr.file_type()) {
                warn!("Path {} backing a file node is no longer a file; got {:?}",
                    path.display(), fs_attr.file_type());
                return Err(KernelError::from_errno(errno::Errno::EIO));
            }
            state.attr = conv::attr_fs_to_fuse(path, inode, state.attr.nlink, &fs_attr);
        }

        Ok(state.attr)
    }
}

impl Node for File {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn file_type_cached(&self) -> fuse::FileType {
        let state = self.state.lock().unwrap();
        state.attr.kind
    }

    fn delete(&self, cache: &Cache) {
        let mut state = self.state.lock().unwrap();
        assert!(
            state.underlying_path.is_some(),
            "Delete already called or trying to delete an explicit mapping");
        cache.delete(state.underlying_path.as_ref().unwrap(), state.attr.kind);
        state.underlying_path = None;
        debug_assert!(state.attr.nlink >= 1);
        state.attr.nlink -= 1;
    }

    fn set_underlying_path(&self, path: &Path, cache: &Cache) {
        let mut state = self.state.lock().unwrap();
        debug_assert!(state.underlying_path.is_some(),
            "Renames should not have been allowed in scaffold or deleted nodes");
        cache.rename(
            state.underlying_path.as_ref().unwrap(), path.to_owned(), state.attr.kind);
        state.underlying_path = Some(PathBuf::from(path));
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        File::getattr_locked(self.inode, &mut state)
    }

    fn getxattr(&self, name: &OsStr) -> NodeResult<Option<Vec<u8>>> {
        let state = self.state.lock().unwrap();
        match &state.underlying_path {
            Some(path) => Ok(xattr::get(path, name)?),
            None => Err(KernelError::from_errno(errno::Errno::ENOENT)),
        }
    }

    fn handle_from(&self, file: fs::File) -> ArcHandle {
        Arc::from(OpenFile::from(self.state.clone(), file))
    }

    fn listxattr(&self) -> NodeResult<xattr::XAttrs> {
        let state = self.state.lock().unwrap();
        match &state.underlying_path {
            Some(path) => Ok(xattr::list(path)?),
            None => Err(KernelError::from_errno(errno::Errno::ENOENT)),
        }
    }

    fn open(&self, flags: u32) -> NodeResult<ArcHandle> {
        let state = self.state.lock().unwrap();

        let options = conv::flags_to_openoptions(flags, self.writable)?;
        let path = state.underlying_path.as_ref().expect(
            "Don't know how to handle a request to reopen a deleted file");
        let file = options.open(&path)?;
        Ok(Arc::from(OpenFile::from(self.state.clone(), file)))
    }

    fn removexattr(&self, name: &OsStr) -> NodeResult<()> {
        let state = self.state.lock().unwrap();
        match &state.underlying_path {
            Some(path) => Ok(xattr::remove(path, name)?),
            None => Err(KernelError::from_errno(errno::Errno::ENOENT)),
        }
    }

    fn setattr(&self, delta: &AttrDelta) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        state.attr = setattr(state.underlying_path.as_ref(), &state.attr, delta)?;
        Ok(state.attr)
    }

    fn setxattr(&self, name: &OsStr, value: &[u8]) -> NodeResult<()> {
        let state = self.state.lock().unwrap();
        match &state.underlying_path {
                Some(path) => Ok(xattr::set(path, name, value)?),
                None => Err(KernelError::from_errno(errno::Errno::ENOENT)),
        }
    }
}
