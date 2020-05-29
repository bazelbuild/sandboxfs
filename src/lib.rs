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

//! Implementation of the sandboxfs FUSE file system.

// Keep these in sync with the list of checks in main.rs.
#![warn(bad_style, missing_docs)]
#![warn(unused, unused_extern_crates, unused_import_braces, unused_qualifications)]
#![warn(unsafe_code)]

// For portability reasons, we need to be able to cast integer values to system-level opaque
// types such as "mode_t".  Because we don't know the size of those integers on the platform we
// are building for, sometimes the casts do widen the values but other times they are no-ops.
#![allow(clippy::identity_conversion)]

// We construct complex structures in multiple places, and allowing for redundant field names
// increases readability.
#![allow(clippy::redundant_field_names)]

#[cfg(feature = "profiling")] extern crate cpuprofiler;
#[macro_use] extern crate failure;
extern crate fuse;
#[macro_use] extern crate log;
extern crate nix;
extern crate serde_derive;
extern crate signal_hook;
#[cfg(test)] extern crate tempfile;
#[cfg(test)] extern crate users;
extern crate threadpool;
extern crate time;
extern crate xattr;

use failure::{Fallible, ResultExt};
use nix::errno::Errno;
use nix::{sys, unistd};
use std::collections::HashMap;
use std::ffi::OsStr;
use std::fmt;
use std::fs;
use std::os::unix::ffi::OsStrExt;
use std::path::{Component, Path, PathBuf};
use std::result::Result;
use std::sync::{Arc, Mutex};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::thread;
use time::Timespec;

mod concurrent;
mod errors;
mod nodes;
mod profiling;
mod reconfig;
#[cfg(test)] mod testutils;

pub use errors::{flatten_causes, KernelError, MappingError};
pub use nodes::{ArcCache, NoCache, PathCache};
pub use profiling::ScopedProfiler;
pub use reconfig::{open_input, open_output};

/// Mapping describes how an individual path within the sandbox is connected to an external path
/// in the underlying file system.
#[derive(Debug, Eq, PartialEq)]
pub struct Mapping {
    path: PathBuf,
    underlying_path: PathBuf,
    writable: bool,
}
impl Mapping {
    /// Creates a new mapping from the individual components.
    ///
    /// `path` is the inside the sandbox's mount point where the `underlying_path` is exposed.
    /// Both must be absolute paths.  `path` must also not contain dot-dot components, though it
    /// may contain dot components and repeated path separators.
    pub fn from_parts(path: PathBuf, underlying_path: PathBuf, writable: bool)
        -> Result<Self, MappingError> {
        if !path.is_absolute() {
            return Err(MappingError::PathNotAbsolute { path });
        }
        let is_normalized = {
            let mut components = path.components();
            assert_eq!(components.next(), Some(Component::RootDir), "Path expected to be absolute");
            let is_not_normal: fn(&Component) -> bool = |c| match c {
                Component::CurDir => panic!("Dot components ought to have been skipped"),
                Component::Normal(_) => false,
                Component::ParentDir | Component::Prefix(_) => true,
                Component::RootDir => panic!("Root directory should have already been handled"),
            };
            components.find(is_not_normal).is_none()
        };
        if !is_normalized {
            return Err(MappingError::PathNotNormalized{ path });
        }

        if !underlying_path.is_absolute() {
            return Err(MappingError::PathNotAbsolute { path: underlying_path });
        }

        Ok(Mapping { path, underlying_path, writable })
    }

    /// Returns true if this is a mapping for the root directory.
    fn is_root(&self) -> bool {
        self.path.parent().is_none()
    }
}

impl fmt::Display for Mapping {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        let writability = if self.writable { "read/write" } else { "read-only" };
        write!(f, "{} -> {} ({})", self.path.display(), self.underlying_path.display(), writability)
    }
}

/// Monotonically-increasing generator of identifiers.
pub struct IdGenerator {
    last_id: AtomicUsize,
}

impl IdGenerator {
    /// Generation number to return to the kernel for any inode number.
    ///
    /// We don't reuse inode numbers throughout the lifetime of a sandboxfs instance and we do not
    /// support FUSE daemon restarts without going through an unmount/mount sequence, so it is OK
    /// to have a constant number here.
    const GENERATION: u64 = 0;

    /// Constructs a new generator that starts at the given value.
    fn new(start_value: u64) -> Self {
        IdGenerator { last_id: AtomicUsize::new(start_value as usize) }
    }

    /// Obtains a new identifier.
    pub fn next(&self) -> u64 {
        let id = self.last_id.fetch_add(1, Ordering::AcqRel);
        // TODO(https://github.com/rust-lang/rust/issues/51577): Drop :: prefix.
        if id >= ::std::u64::MAX as usize {
            panic!("Ran out of identifiers");
        }
        id as u64
    }
}

/// FUSE file system implementation of sandboxfs.
struct SandboxFS {
    /// Monotonically-increasing generator of identifiers for this file system instance.
    ids: Arc<IdGenerator>,

    /// Mapping of inode numbers to in-memory nodes that tracks all files known by sandboxfs.
    nodes: Arc<Mutex<HashMap<u64, nodes::ArcNode>>>,

    /// Mapping of handle numbers to file open handles.
    handles: Arc<Mutex<HashMap<u64, nodes::ArcHandle>>>,

    /// Cache of sandboxfs nodes indexed by their underlying path.
    cache: ArcCache,

    /// How long to tell the kernel to cache file metadata for.
    ttl: Timespec,

    /// Whether support for xattrs is enabled or not.
    xattrs: bool,
}

/// A view of a `SandboxFS` instance to allow for concurrent reconfigurations.
///
/// This structure exists because `fuse::mount` takes ownership of the file system so there is no
/// way for us to share that instance across threads.  Instead, we construct a reduced view of the
/// fields we need for reconfiguration and pass those across threads.
#[derive(Clone)]
struct ReconfigurableSandboxFS {
    /// The root node of the file system, on which to apply all reconfiguration operations.
    root: nodes::ArcNode,

    /// Monotonically-increasing generator of identifiers for this file system instance.
    ids: Arc<IdGenerator>,

    /// Mapping of inode numbers to in-memory nodes that tracks all files known by sandboxfs.
    nodes: Arc<Mutex<HashMap<u64, nodes::ArcNode>>>,

    /// Cache of sandboxfs nodes indexed by their underlying path.
    cache: ArcCache,
}

/// Splits an absolute path into components, stripping the first root component.
///
/// If the input path was the root directory, the result is an empty set of components.  Otherwise,
/// the first returned component is always a `Component::Normal` component.
// TODO(jmmv): The callers of this function should be able to operate on the iterator alone,
// without having to materialize the components into a vector.  Doing so requires changing all
// directory traversal functions to also operate on a `Components` iterator (possibly a `Peekable`
// one) so it's not clear to me that that's going to be faster; need to measure.
fn split_abs_path(path: &Path) -> Vec<Component> {
    let mut components = path.components();
    let root = components.next().expect("Should have been called on an absolute path only");
    debug_assert_eq!(Component::RootDir, root,
        "Paths in mappings are always absolute but got {:?}", path);
    components.collect::<Vec<_>>()
}

/// Applies a mapping to the given root node.
///
/// This code is shared by the application of `--mapping` flags and by the application of new
/// mappings as part of a reconfiguration operation.  We want both processes to behave identically.
fn apply_mapping(mapping: &Mapping, root: &dyn nodes::Node, ids: &IdGenerator,
    cache: &dyn nodes::Cache) -> Fallible<nodes::ArcNode> {
    let components = split_abs_path(&mapping.path);

    // The input `root` node is an existing node that corresponds to the root.  If we don't find
    // any path components in the given mapping, it means we are trying to remap that same node.
    ensure!(!components.is_empty(), "Root can be mapped at most once");

    root.map(&components, &mapping.underlying_path, mapping.writable, &ids, cache)
}

/// Creates the initial node hierarchy based on a collection of `mappings`.
fn create_root(mappings: &[Mapping], ids: &IdGenerator, cache: &dyn nodes::Cache)
    -> Fallible<nodes::ArcNode> {
    let now = time::get_time();

    let (root, rest) = if mappings.is_empty() {
        (nodes::Dir::new_empty(ids.next(), None, now), mappings)
    } else {
        let first = &mappings[0];
        if first.is_root() {
            let fs_attr = fs::symlink_metadata(&first.underlying_path)
                .with_context(|_| format!("Failed to map root: stat failed for {:?}",
                    &first.underlying_path))?;
            ensure!(fs_attr.is_dir(), "Failed to map root: {:?} is not a directory",
                    &first.underlying_path);
            (nodes::Dir::new_mapped(ids.next(), &first.underlying_path, &fs_attr, first.writable),
                &mappings[1..])
        } else {
            (nodes::Dir::new_empty(ids.next(), None, now), mappings)
        }
    };

    for mapping in rest {
        apply_mapping(mapping, root.as_ref(), ids, cache)
            .with_context(|_| format!("Cannot map '{}'", mapping))?;
    }

    Ok(root)
}

impl SandboxFS {
    /// Creates a new `SandboxFS` instance.
    fn create(mappings: &[Mapping], ttl: Timespec, cache: ArcCache, xattrs: bool)
        -> Fallible<SandboxFS> {
        let ids = IdGenerator::new(fuse::FUSE_ROOT_ID);

        let mut nodes = HashMap::new();
        let root = create_root(mappings, &ids, cache.as_ref())?;
        assert_eq!(fuse::FUSE_ROOT_ID, root.inode());
        nodes.insert(root.inode(), root);

        Ok(SandboxFS {
            ids: Arc::from(ids),
            nodes: Arc::from(Mutex::from(nodes)),
            handles: Arc::from(Mutex::from(HashMap::new())),
            cache: cache,
            ttl: ttl,
            xattrs: xattrs,
        })
    }

    /// Creates a reconfigurable view of this file system, to safely pass across threads.
    fn reconfigurable(&mut self) -> ReconfigurableSandboxFS {
        ReconfigurableSandboxFS {
            root: self.find_node(fuse::FUSE_ROOT_ID).expect("Root node must always exist"),
            ids: self.ids.clone(),
            nodes: self.nodes.clone(),
            cache: self.cache.clone(),
        }
    }

    /// Gets a node given its `inode`.
    fn find_node(&mut self, inode: u64) -> nodes::NodeResult<nodes::ArcNode> {
        let nodes = self.nodes.lock().unwrap();
        match nodes.get(&inode) {
            Some(node) => Ok(node.clone()),
            None => Err(KernelError::from_errno(Errno::ENOENT)),
        }
    }

    /// Gets a node given its `inode` and ensures it is writable.
    fn find_writable_node(&mut self, inode: u64) -> nodes::NodeResult<nodes::ArcNode> {
        let node = self.find_node(inode)?;
        if !node.writable() {
            Err(KernelError::from_errno(Errno::EPERM))
        } else {
            Ok(node.clone())
        }
    }

    /// Gets a handle given its identifier.
    ///
    /// We assume that the identifier is valid and that we have a known handle for it; otherwise,
    /// we crash.  The rationale for this is that this function is always called on handles
    /// requested by the kernel, and we can trust that the kernel will only ever ask us for handles
    /// numbers we have previously told it about.
    fn find_handle(&mut self, fh: u64) -> nodes::ArcHandle {
        let handles = self.handles.lock().unwrap();
        handles.get(&fh).expect("Kernel requested unknown handle").clone()
    }

    /// Tracks a new file handle and assigns an identifier to it.
    fn insert_handle(&mut self, handle: nodes::ArcHandle) -> u64 {
        let fh = self.ids.next();
        let mut handles = self.handles.lock().unwrap();
        debug_assert!(!handles.contains_key(&fh));
        handles.insert(fh, handle);
        fh
    }

    /// Tracks a node, which may already be known.
    fn insert_node(&mut self, node: nodes::ArcNode) {
        let mut nodes = self.nodes.lock().unwrap();
        nodes.entry(node.inode()).or_insert(node);
    }

    /// Same as `create` but leaves the handling of the `fuse::Reply` to the caller.
    fn create2(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, mode: u32, flags: u32)
        -> nodes::NodeResult<(fuse::FileAttr, u64)> {
        let dir_node = self.find_writable_node(parent)?;
        let (node, handle, attr) = dir_node.create(
            name, nix_uid(req), nix_gid(req), mode, flags, &self.ids, self.cache.as_ref())?;
        self.insert_node(node);
        let fh = self.insert_handle(handle);
        Ok((attr, fh))
    }

    /// Same as `getattr` but leaves the handling of the `fuse::Reply` to the caller.
    fn getattr2(&mut self, inode: u64) -> nodes::NodeResult<fuse::FileAttr> {
        let node = self.find_node(inode)?;
        node.getattr()
    }

    /// Same as `lookup` but leaves the handling of the `fuse::Reply` to the caller.
    fn lookup2(&mut self, parent: u64, name: &OsStr) -> nodes::NodeResult<fuse::FileAttr> {
        let dir_node = self.find_node(parent)?;
        let (node, attr) = dir_node.lookup(name, &self.ids, self.cache.as_ref())?;
        let mut nodes = self.nodes.lock().unwrap();
        if !nodes.contains_key(&node.inode()) {
            nodes.insert(node.inode(), node);
        }
        Ok(attr)
    }

    /// Same as `mkdir` but leaves the handling of the `fuse::Reply` to the caller.
    fn mkdir2(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, mode: u32)
        -> nodes::NodeResult<fuse::FileAttr> {
        let dir_node = self.find_writable_node(parent)?;
        let (node, attr) = dir_node.mkdir(
            name, nix_uid(req), nix_gid(req), mode, &self.ids, self.cache.as_ref())?;
        self.insert_node(node);
        Ok(attr)
    }

    /// Same as `mknod` but leaves the handling of the `fuse::Reply` to the caller.
    fn mknod2(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, mode: u32, rdev: u32)
        -> nodes::NodeResult<fuse::FileAttr> {
        let dir_node = self.find_writable_node(parent)?;

        let (node, attr) = dir_node.mknod(
            name, nix_uid(req), nix_gid(req), mode, rdev, &self.ids, self.cache.as_ref())?;
        self.insert_node(node);
        Ok(attr)
    }

    /// Same as `open` and `opendir` but leaves the handling of the `fuse::Reply` to the caller.
    fn open2(&mut self, inode: u64, flags: u32) -> nodes::NodeResult<u64> {
        let node = self.find_node(inode)?;
        let handle = node.open(flags)?;
        Ok(self.insert_handle(handle))
    }

    /// Same as `readlink` but leaves the handling of the `fuse::Reply` to the caller.
    fn readlink2(&mut self, inode: u64) -> nodes::NodeResult<PathBuf> {
        let node = self.find_node(inode)?;
        node.readlink()
    }

    /// Same as `release` and `releasedir` but leaves the handling of the `fuse::Reply` to the
    /// caller.
    fn release2(&mut self, fh: u64) {
        let mut handles = self.handles.lock().unwrap();
        handles.remove(&fh).expect("Kernel tried to release an unknown handle");
    }

    /// Same as `rename` but leaves the handling of the `fuse::Reply` to the caller.
    fn rename2(&mut self, parent: u64, name: &OsStr, new_parent: u64, new_name: &OsStr)
        -> nodes::NodeResult<()> {
        let dir_node = self.find_writable_node(parent)?;
        if parent == new_parent {
            dir_node.rename(name, new_name, self.cache.as_ref())
        } else {
            let new_dir_node = self.find_writable_node(new_parent)?;
            dir_node.rename_and_move_source(name, new_dir_node, new_name, self.cache.as_ref())
        }
    }

    /// Same as `rmdir` but leaves the handling of the `fuse::Reply` to the caller.
    fn rmdir2(&mut self, parent: u64, name: &OsStr) -> nodes::NodeResult<()> {
        let dir_node = self.find_writable_node(parent)?;
        dir_node.rmdir(name, self.cache.as_ref())
    }

    /// Same as `setattr` but leaves the handling of the `fuse::Reply` to the caller.
    #[allow(clippy::too_many_arguments)]
    fn setattr2(&mut self, inode: u64, mode: Option<u32>, uid: Option<u32>,
        gid: Option<u32>, size: Option<u64>, atime: Option<Timespec>, mtime: Option<Timespec>)
        -> nodes::NodeResult<fuse::FileAttr> {
        let node = self.find_writable_node(inode)?;
        let values = nodes::AttrDelta {
            mode: mode.map(|m| sys::stat::Mode::from_bits_truncate(m as sys::stat::mode_t)),
            uid: uid.map(unistd::Uid::from_raw),
            gid: gid.map(unistd::Gid::from_raw),
            atime: atime.map(nodes::conv::timespec_to_timeval),
            mtime: mtime.map(nodes::conv::timespec_to_timeval),
            size: size,
        };
        node.setattr(&values)
    }

    /// Same as `symlink` but leaves the handling of the `fuse::Reply` to the caller.
    fn symlink2(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, link: &Path)
        -> nodes::NodeResult<fuse::FileAttr> {
        let dir_node = self.find_writable_node(parent)?;
        let (node, attr) = dir_node.symlink(
            name, link, nix_uid(req), nix_gid(req), &self.ids, self.cache.as_ref())?;
        self.insert_node(node);
        Ok(attr)
    }

    /// Same as `unlink` but leaves the handling of the `fuse::Reply` to the caller.
    fn unlink2(&mut self, parent: u64, name: &OsStr) -> nodes::NodeResult<()> {
        let dir_node = self.find_writable_node(parent)?;
        dir_node.unlink(name, self.cache.as_ref())
    }

    /// Same as `setxattr` but leaves the handling of the `fuse::Reply` to the caller.
    fn setxattr2(&mut self, inode: u64, name: &OsStr, value: &[u8]) -> nodes::NodeResult<()> {
        let node = self.find_writable_node(inode)?;
        node.setxattr(name, value)
    }

    /// Same as `getxattr` but leaves the handling of the `fuse::Reply` to the caller.
    fn getxattr2(&mut self, inode: u64, name: &OsStr) -> nodes::NodeResult<Vec<u8>> {
        let node = self.find_node(inode)?;
        match node.getxattr(name) {
            Ok(None) => {
                #[cfg(target_os = "linux")]
                let code = Errno::ENODATA;

                #[cfg(target_os = "macos")]
                let code = Errno::ENOATTR;

                #[cfg(not(any(target_os = "linux", target_os = "macos")))]
                compile_error!("Don't know what error to return on a missing getxattr");

                Err(KernelError::from_errno(code))
            },
            Ok(Some(value)) => Ok(value),
            Err(e) => Err(e),
        }
    }

    /// Same as `listxattr` but leaves the handling of the `fuse::Reply` to the caller.
    fn listxattr2(&mut self, inode: u64) -> nodes::NodeResult<Option<xattr::XAttrs>> {
        let node = self.find_node(inode)?;
        node.listxattr()
    }

    /// Same as `removexattr` but leaves the handling of the `fuse::Reply` to the caller.
    fn removexattr2(&mut self, inode: u64, name: &OsStr) -> nodes::NodeResult<()> {
        let node = self.find_writable_node(inode)?;
        node.removexattr(name)
    }
}

/// Creates a file `path` with the given `uid`/`gid` pair.
///
/// The file is created via the `create` lambda, which can create any type of file it wishes.  The
/// `delete` lambda should match this creation and allow the deletion of the file, and this is used
/// as a cleanup function when the ownership cannot be successfully changed.
fn create_as<T, E: From<Errno> + fmt::Display, P: AsRef<Path>>(
    path: &P, uid: unistd::Uid, gid: unistd::Gid,
    create: impl Fn(&P) -> Result<T, E>,
    delete: impl Fn(&P) -> Result<(), E>)
    -> Result<T, E> {

    let result = create(path)?;

    unistd::fchownat(
        None, path.as_ref(), Some(uid), Some(gid), unistd::FchownatFlags::NoFollowSymlink)
        .map_err(|e| {
            let chown_errno = match e {
                nix::Error::Sys(chown_errno) => chown_errno,
                unknown_chown_error => {
                    warn!("fchownat({}) failed with unexpected non-errno error: {:?}",
                          path.as_ref().display(), unknown_chown_error);
                    Errno::EIO
                },
            };

            if let Err(e) = delete(path) {
                warn!("Cannot delete created file {} after failing to change ownership: {}",
                      path.as_ref().display(), e);
            }

            chown_errno
        })?;

    Ok(result)
}

/// Returns a `unistd::Uid` representation of the UID in a `fuse::Request`.
fn nix_uid(req: &fuse::Request) -> unistd::Uid {
    unistd::Uid::from_raw(req.uid() as u32)
}

/// Returns a `unistd::Gid` representation of the GID in a `fuse::Request`.
fn nix_gid(req: &fuse::Request) -> unistd::Gid {
    unistd::Gid::from_raw(req.gid() as u32)
}

/// Converts a collection of extended attribute names into a raw vector of null-terminated strings.
///
// TODO(jmmv): This conversion is unnecessary.  `Xattrs` has the raw representation of the extended
// attributes, which we could forward to the kernel directly.
fn xattrs_to_u8(xattrs: xattr::XAttrs) -> Vec<u8> {
    let mut length = 0;
    for xa in xattrs.clone().into_iter() {
        length += xa.len() + 1;
    }
    let mut data = Vec::with_capacity(length);
    for xa in xattrs.into_iter() {
        for b in xa.as_bytes() {
            data.push(*b);
        }
        data.push(0);
    }
    data
}

/// Responds to a successful xattr get or list request.
///
/// If `size` is zero, the kernel wants to know the length of `value`.  Otherwise, we are being
/// asked for the actual value, which should not be longer than `size`.
fn reply_xattr(size: u32, value: &[u8], reply: fuse::ReplyXattr) {
    if size == 0 {
        if value.len() > std::u32::MAX as usize {
            warn!("xattr data too long ({} bytes); cannot reply", value.len());
            reply.error(Errno::EIO as i32);
        } else {
            reply.size(value.len() as u32);
        }
    } else if (size as usize) < value.len() {
        reply.error(Errno::ERANGE as i32);
    } else {
        reply.data(value);
    }
}

impl fuse::Filesystem for SandboxFS {
    fn create(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, mode: u32, flags: u32,
        reply: fuse::ReplyCreate) {
        match self.create2(req, parent, name, mode, flags) {
            Ok((attr, fh)) => reply.created(&self.ttl, &attr, IdGenerator::GENERATION, fh, 0),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn getattr(&mut self, _req: &fuse::Request, inode: u64, reply: fuse::ReplyAttr) {
        match self.getattr2(inode) {
            Ok(attr) => reply.attr(&self.ttl, &attr),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn link(&mut self, _req: &fuse::Request, _inode: u64, _newparent: u64, _newname: &OsStr,
        reply: fuse::ReplyEntry) {
        // We don't support hardlinks at this point.
        reply.error(Errno::EPERM as i32);
    }

    fn lookup(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, reply: fuse::ReplyEntry) {
        match self.lookup2(parent, name) {
            Ok(attr) => reply.entry(&self.ttl, &attr, IdGenerator::GENERATION),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn mkdir(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, mode: u32,
        reply: fuse::ReplyEntry) {
        match self.mkdir2(req, parent, name, mode) {
            Ok(attr) => reply.entry(&self.ttl, &attr, IdGenerator::GENERATION),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn mknod(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, mode: u32, rdev: u32,
        reply: fuse::ReplyEntry) {
        match self.mknod2(req, parent, name, mode, rdev) {
            Ok(attr) => reply.entry(&self.ttl, &attr, IdGenerator::GENERATION),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn open(&mut self, _req: &fuse::Request, inode: u64, flags: u32, reply: fuse::ReplyOpen) {
        match self.open2(inode, flags) {
            Ok(fh) => reply.opened(fh, 0),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn opendir(&mut self, _req: &fuse::Request, inode: u64, flags: u32, reply: fuse::ReplyOpen) {
        match self.open2(inode, flags) {
            Ok(fh) => reply.opened(fh, 0),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn read(&mut self, _req: &fuse::Request, _inode: u64, fh: u64, offset: i64, size: u32,
        reply: fuse::ReplyData) {
        let handle = self.find_handle(fh);

        match handle.read(offset, size) {
            Ok(data) => reply.data(&data),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn readdir(&mut self, _req: &fuse::Request, _inode: u64, handle: u64, offset: i64,
               mut reply: fuse::ReplyDirectory) {
        let handle = self.find_handle(handle);
        match handle.readdir(&self.ids, self.cache.as_ref(), offset, &mut reply) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn readlink(&mut self, _req: &fuse::Request, inode: u64, reply: fuse::ReplyData) {
        match self.readlink2(inode) {
            Ok(target) => reply.data(target.as_os_str().as_bytes()),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn release(&mut self, _req: &fuse::Request, _inode: u64, fh: u64, _flags: u32, _lock_owner: u64,
        _flush: bool, reply: fuse::ReplyEmpty) {
        self.release2(fh);
        reply.ok();
    }

    fn releasedir(&mut self, _req: &fuse::Request, _inode: u64, fh: u64, _flags: u32,
        reply: fuse::ReplyEmpty) {
        self.release2(fh);
        reply.ok();
    }

    fn rename(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, new_parent: u64,
        new_name: &OsStr, reply: fuse::ReplyEmpty) {
        match self.rename2(parent, name, new_parent, new_name) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn rmdir(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, reply: fuse::ReplyEmpty) {
        match self.rmdir2(parent, name) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn setattr(&mut self, _req: &fuse::Request, inode: u64, mode: Option<u32>, uid: Option<u32>,
        gid: Option<u32>, size: Option<u64>, atime: Option<Timespec>, mtime: Option<Timespec>,
        _fh: Option<u64>, _crtime: Option<Timespec>, _chgtime: Option<Timespec>,
        _bkuptime: Option<Timespec>, _flags: Option<u32>, reply: fuse::ReplyAttr) {
        match self.setattr2(inode, mode, uid, gid, size, atime, mtime) {
            Ok(attr) => reply.attr(&self.ttl, &attr),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn symlink(&mut self, req: &fuse::Request, parent: u64, name: &OsStr, link: &Path,
        reply: fuse::ReplyEntry) {
        match self.symlink2(req, parent, name, link) {
            Ok(attr) => reply.entry(&self.ttl, &attr, IdGenerator::GENERATION),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn unlink(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, reply: fuse::ReplyEmpty) {
        match self.unlink2(parent, name) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn write(&mut self, _req: &fuse::Request, _inode: u64, fh: u64, offset: i64, data: &[u8],
        _flags: u32, reply: fuse::ReplyWrite) {
        let handle = self.find_handle(fh);

        match handle.write(offset, data) {
            Ok(size) => reply.written(size),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn setxattr(&mut self, _req: &fuse::Request<'_>, inode: u64, name: &OsStr, value: &[u8],
        _flags: u32, _position: u32, reply: fuse::ReplyEmpty) {
        if !self.xattrs {
            reply.error(Errno::ENOSYS as i32);
            return;
        }

        match self.setxattr2(inode, name, value) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn getxattr(&mut self, _req: &fuse::Request<'_>, inode: u64, name: &OsStr, size: u32,
        reply: fuse::ReplyXattr) {
        if !self.xattrs {
            reply.error(Errno::ENOSYS as i32);
            return;
        }

        match self.getxattr2(inode, name) {
            Ok(value) => reply_xattr(size, value.as_slice(), reply),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn listxattr(&mut self, _req: &fuse::Request<'_>, inode: u64, size: u32,
        reply: fuse::ReplyXattr) {
        if !self.xattrs {
            reply.error(Errno::ENOSYS as i32);
            return;
        }

        match self.listxattr2(inode) {
            Ok(Some(xattrs)) => reply_xattr(size, xattrs_to_u8(xattrs).as_slice(), reply),
            Ok(None) => {
                if size == 0 {
                    reply.size(0);
                } else {
                    reply.data(&[]);
                }
            },
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn removexattr(&mut self, _req: &fuse::Request<'_>, inode: u64, name: &OsStr,
        reply: fuse::ReplyEmpty) {
        if !self.xattrs {
            reply.error(Errno::ENOSYS as i32);
            return;
        }

        match self.removexattr2(inode, name) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }
}

impl reconfig::ReconfigurableFS for ReconfigurableSandboxFS {
    fn create_sandbox(&self, id: &str, mut mappings: &[Mapping]) -> Fallible<()> {
        // Special-case the first mapping if it is for the "root" directory.  We know that this
        // mapping, if present, must come first (as otherwise it will fail when applied later on
        // anyway).  But if it is first, we must treat it as if we were mapping the "root" itself.
        let root_node = match mappings.get(0) {
            Some(mapping) => {
                if mapping.path.as_path() == Path::new(&"/") {
                    let path = reconfig::make_path(id, mapping.path.clone())?;
                    mappings = &mappings[1..];
                    let m = Mapping::from_parts(
                        path, mapping.underlying_path.clone(), mapping.writable)?;
                    apply_mapping(&m, self.root.as_ref(), self.ids.as_ref(), self.cache.as_ref())
                        .with_context(|_| format!("Cannot map '{}'", mapping))?
                } else {
                    self.root.find_subdir(OsStr::new(id), self.ids.as_ref())?
                }
            },
            None => self.root.find_subdir(OsStr::new(id), self.ids.as_ref())?,
        };

        // TODO(jmmv): Even though we don't hold the root lock any longer, this *still* is very
        // inefficient because keep locking/unlocking the top directory for every mapping.  Should
        // pass the list of mappings down to the `map` operation... but that'd only fix this issue
        // for the top-level directory; what about all intermediate directories for all mappings?
        for mapping in mappings {
            apply_mapping(
                mapping, root_node.clone().as_ref(), self.ids.as_ref(), self.cache.as_ref())
                    .with_context(|_| format!("Cannot map '{}'", mapping))?;
        }
        Ok(())
    }

    fn destroy_sandbox(&self, id: &str) -> Fallible<()> {
        let mut inodes = vec!();
        let result = self.root.unmap_subdir(OsStr::new(id), &mut inodes);

        let mut nodes = self.nodes.lock().unwrap();
        for inode in inodes {
            nodes.remove(&inode);
        }

        result
    }
}

/// Mounts a new sandboxfs instance on the given `mount_point` and maps all `mappings` within it.
#[allow(clippy::too_many_arguments)]
pub fn mount(mount_point: &Path, options: &[&str], mappings: &[Mapping], ttl: Timespec,
    cache: ArcCache, xattrs: bool, input: fs::File, output: fs::File, threads: usize)
    -> Fallible<()> {
    let mut os_options = options.iter().map(AsRef::as_ref).collect::<Vec<&OsStr>>();

    // Delegate permissions checks to the kernel for efficiency and to avoid having to implement
    // them on our own.
    os_options.push(OsStr::new("-o"));
    os_options.push(OsStr::new("default_permissions"));

    let mut fs = SandboxFS::create(mappings, ttl, cache, xattrs)?;
    let reconfigurable_fs = fs.reconfigurable();
    info!("Mounting file system onto {:?}", mount_point);

    let (signals, mut session) = {
        let installer = concurrent::SignalsInstaller::prepare();
        let session = fuse::Session::new(fs, &mount_point, &os_options)?;
        let signals = installer.install(PathBuf::from(mount_point))?;
        (signals, session)
    };

    let config_handler = {
        let mut input = concurrent::ShareableFile::from(input);
        let reader = input.reader()?;
        let handler = thread::spawn(move || {
            match reconfig::run_loop(reader, output, threads, &reconfigurable_fs) {
                Ok(()) => info!(
                    "Reached end of reconfiguration input; file system mappings are now frozen"),
                Err(e) => warn!("Reconfigurations stopped due to internal error: {}", e),
            }
        });

        session.run()?;
        handler
    };
    // The input must be closed to let the reconfiguration thread to exit, which then lets the join
    // operation below complete, hence the scope above.
    if let Some(signo) = signals.caught() {
        info!("Caught signal {}", signo);
        return Err(format_err!("Caught signal {}", signo));
    }

    match config_handler.join() {
        Ok(_) => Ok(()),
        Err(_) => Err(format_err!("Reconfiguration thread panicked")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::MetadataExt;
    use tempfile::tempdir;

    #[test]
    fn test_mapping_new_ok() {
        let mapping = Mapping::from_parts(
            PathBuf::from("/foo/.///bar"),  // Must be absolute and normalized.
            PathBuf::from("/bar/./baz/../abc"),  // Must be absolute but needn't be normalized.
            false).unwrap();
        assert_eq!(PathBuf::from("/foo/bar"), mapping.path);
        assert_eq!(PathBuf::from("/bar/baz/../abc"), mapping.underlying_path);
        assert!(!mapping.writable);
    }

    #[test]
    fn test_mapping_new_path_is_not_absolute() {
        let err = Mapping::from_parts(
            PathBuf::from("foo"), PathBuf::from("/bar"), false).unwrap_err();
        assert_eq!(MappingError::PathNotAbsolute { path: PathBuf::from("foo") }, err);
    }

    #[test]
    fn test_mapping_new_path_is_not_normalized() {
        let trailing_dotdot = PathBuf::from("/foo/..");
        assert_eq!(
            MappingError::PathNotNormalized { path: trailing_dotdot.clone() },
            Mapping::from_parts(trailing_dotdot, PathBuf::from("/bar"), false).unwrap_err());

        let intermediate_dotdot = PathBuf::from("/foo/../bar/baz");
        assert_eq!(
            MappingError::PathNotNormalized { path: intermediate_dotdot.clone() },
            Mapping::from_parts(intermediate_dotdot, PathBuf::from("/bar"), true).unwrap_err());
    }

    #[test]
    fn test_mapping_new_underlying_path_is_not_absolute() {
        let err = Mapping::from_parts(
            PathBuf::from("/foo"), PathBuf::from("bar"), false).unwrap_err();
        assert_eq!(MappingError::PathNotAbsolute { path: PathBuf::from("bar") }, err);
    }

    #[test]
    fn test_mapping_is_root() {
        let irrelevant = PathBuf::from("/some/place");
        assert!(Mapping::from_parts(
            PathBuf::from("/"), irrelevant.clone(), false).unwrap().is_root());
        assert!(Mapping::from_parts(
            PathBuf::from("///"), irrelevant.clone(), false).unwrap().is_root());
        assert!(Mapping::from_parts(
            PathBuf::from("/./"), irrelevant.clone(), false).unwrap().is_root());
        assert!(!Mapping::from_parts(
            PathBuf::from("/a"), irrelevant.clone(), false).unwrap().is_root());
        assert!(!Mapping::from_parts(
            PathBuf::from("/a/b"), irrelevant, false).unwrap().is_root());
    }

    #[test]
    fn id_generator_ok() {
        let ids = IdGenerator::new(10);
        assert_eq!(10, ids.next());
        assert_eq!(11, ids.next());
        assert_eq!(12, ids.next());
    }

    #[test]
    #[should_panic(expected = "Ran out of identifiers")]
    fn id_generator_exhaustion() {
        let ids = IdGenerator::new(std::u64::MAX);
        ids.next();  // OK, still at limit.
        ids.next();  // Should panic.
    }

    #[test]
    fn test_split_abs_path() {
        let empty: [Component; 0] = [];
        assert_eq!(
            &empty,
            split_abs_path(&Path::new("/")).as_slice());
        assert_eq!(
            &[Component::Normal(OsStr::new("foo"))],
            split_abs_path(&Path::new("/foo")).as_slice());
        assert_eq!(
            &[Component::Normal(OsStr::new("foo")), Component::Normal(OsStr::new("bar"))],
            split_abs_path(&Path::new("/foo/bar")).as_slice());
    }

    fn do_create_as_ok_test(uid: unistd::Uid, gid: unistd::Gid) {
        let root = tempdir().unwrap();
        let file = root.path().join("dir");
        create_as(&file, uid, gid, |p| fs::create_dir(&p), |p| fs::remove_dir(&p)).unwrap();
        let fs_attr = fs::symlink_metadata(&file).unwrap();
        assert_eq!((uid.as_raw(), gid.as_raw()), (fs_attr.uid(), fs_attr.gid()));
    }

    #[test]
    fn create_as_self() {
        do_create_as_ok_test(unistd::Uid::current(), unistd::Gid::current());
    }

    #[test]
    fn create_as_other() {
        if !unistd::Uid::current().is_root() {
            info!("Test requires root privileges; skipping");
            return;
        }

        let config = testutils::Config::get();
        match config.unprivileged_user {
            Some(user) => {
                let uid = unistd::Uid::from_raw(user.uid());
                let gid = unistd::Gid::from_raw(user.primary_group_id());
                do_create_as_ok_test(uid, gid);
            },
            None => {
                panic!("UNPRIVILEGED_USER must be set when running as root for this test to run");
            }
        }
    }

    #[test]
    fn create_as_create_error_wins_over_delete_error() {
        let path = PathBuf::from("irrelevant");
        let err = create_as(
            &path, unistd::Uid::current(), unistd::Gid::current(),
            |_| Err::<(), nix::Error>(nix::Error::from_errno(Errno::EPERM)),
            |_| Err::<(), nix::Error>(nix::Error::from_errno(Errno::ENOENT))).unwrap_err();
        assert_eq!(nix::Error::from_errno(Errno::EPERM), err);
    }

    #[test]
    fn create_as_file_deleted_if_chown_fails() {
        if unistd::Uid::current().is_root() {
            info!("Test requires non-root privileges; skipping");
            return;
        }

        let other_uid = unistd::Uid::from_raw(unistd::Uid::current().as_raw() + 1);
        let gid = unistd::Gid::current();

        let root = tempdir().unwrap();
        let file = root.path().join("dir");
        create_as(&file, other_uid, gid, |p| fs::create_dir(&p), |p| fs::remove_dir(&p))
            .unwrap_err();
        fs::symlink_metadata(&file).unwrap_err();
    }
}
