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

#[macro_use] extern crate failure;
extern crate fuse;
#[macro_use] extern crate log;
extern crate nix;
extern crate serde_derive;
extern crate signal_hook;
#[cfg(test)] extern crate tempfile;
extern crate time;

use failure::{Fallible, Error, ResultExt};
use nix::errno::Errno;
use nix::{sys, unistd};
use std::collections::HashMap;
use std::ffi::OsStr;
use std::fmt;
use std::fs;
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::path::{Component, Path, PathBuf};
use std::result::Result;
use std::sync::{Arc, Mutex};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::thread;
use time::Timespec;

mod concurrent;
mod nodes;
mod reconfig;
#[cfg(test)] mod testutils;

/// An error indicating that a mapping specification (coming from the command line or from a
/// reconfiguration operation) is invalid.
#[derive(Debug, Eq, Fail, PartialEq)]
pub enum MappingError {
    /// A path was required to be absolute but wasn't.
    #[fail(display = "path {:?} is not absolute", path)]
    PathNotAbsolute {
        /// The invalid path.
        path: PathBuf,
    },

    /// A path contains non-normalized components (like "..").
    #[fail(display = "path {:?} is not normalized", path)]
    PathNotNormalized {
        /// The invalid path.
        path: PathBuf,
    },
}

/// Flattens all causes of an error into a single string.
pub fn flatten_causes(err: &Error) -> String {
    err.iter_chain().fold(String::new(), |flattened, cause| {
        let flattened = if flattened.is_empty() {
            flattened
        } else {
            flattened + ": "
        };
        flattened + &format!("{}", cause)
    })
}

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
            let is_normal: fn(&Component) -> bool = |c| match c {
                Component::CurDir => panic!("Dot components ought to have been skipped"),
                Component::Normal(_) => true,
                Component::ParentDir | Component::Prefix(_) => false,
                Component::RootDir => panic!("Root directory should have already been handled"),
            };
            components.skip_while(is_normal).next().is_none()
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

/// Cache of sandboxfs nodes indexed by their underlying path.
///
/// This cache is critical to offer good performance during reconfigurations: if the identity of an
/// underlying file changes across reconfigurations, the kernel will think it's a different file
/// (even if it may not be) and will therefore not be able to take advantage of any caches.  You
/// would think that avoiding kernel cache invalidations during the reconfiguration itself (e.g. if
/// file `A` was mapped and is still mapped now, don't invalidate it) would be sufficient to avoid
/// this problem, but it's not: `A` could be mapped, then unmapped, and then remapped again in three
/// different reconfigurations, and we'd still not want to lose track of it.
///
/// Nodes should be inserted in this cache at creation time and removed from it when explicitly
/// deleted by the user (because there is a chance they'll be recreated, and at that point we truly
/// want to reload the data from disk).
///
/// TODO(jmmv): There currently is no cache expiration, which means that memory usage can grow
/// unboundedly.  A preliminary attempt at expiring cache entries on a node's forget handler sounded
/// promising (because then cache expiration would be delegated to the kernel)... but, on Linux, the
/// kernel seems to be calling this very eagerly, rendering our cache useless.  I did not track down
/// what exactly triggered the forget notifications though.
#[derive(Default)]
pub struct Cache {
    entries: Mutex<HashMap<PathBuf, nodes::ArcNode>>,
}

impl Cache {
    /// Gets a mapped node from the cache or creates a new one if not yet cached.
    ///
    /// The returned node represents the given underlying path uniquely.  If creation is needed, the
    /// created node uses the given type and writable settings.
    pub fn get_or_create(&self, ids: &IdGenerator, underlying_path: &Path, attr: &fs::Metadata,
        writable: bool) -> nodes::ArcNode {
        if attr.is_dir() {
            // Directories cannot be cached because they contain entries that are created only
            // in memory based on the mappings configuration.
            //
            // TODO(jmmv): Actually, they *could* be cached, but it's hard.  Investigate doing so
            // after quantifying how much it may benefit performance.
            return nodes::Dir::new_mapped(ids.next(), underlying_path, attr, writable);
        }

        let mut entries = self.entries.lock().unwrap();

        if let Some(node) = entries.get(underlying_path) {
            if node.writable() == writable {
                // We have a match from the cache!  Return it immediately.
                //
                // It is tempting to ensure that the type of the cached node matches the type we
                // want to return based on the metadata we have now in `attr`... but doing so does
                // not really prevent problems: the type of the underlying file can change at any
                // point in time.  We could check this here and the type could change immediately
                // afterwards behind our backs, so don't bother.
                return node.clone();
            }

            // We had a match... but node writability has changed; recreate the node.
            //
            // You may wonder why we care about this and not the file type as described above: the
            // reason is that the writability property is a setting of the mappings, not a property
            // of the underlying files, and thus it's a setting that we fully control and must keep
            // correct across reconfigurations or across different mappings of the same files.
            info!("Missed node caching opportunity because writability has changed for {:?}",
                underlying_path)
        }

        let node: nodes::ArcNode = if attr.is_dir() {
            panic!("Directory entries cannot be cached and are handled above");
        } else if attr.file_type().is_symlink() {
            nodes::Symlink::new_mapped(ids.next(), underlying_path, attr, writable)
        } else {
            nodes::File::new_mapped(ids.next(), underlying_path, attr, writable)
        };
        entries.insert(underlying_path.to_path_buf(), node.clone());
        node
    }

    /// Deletes the entry `path` from the cache.
    ///
    /// The `file_type` corresponds to the type of the mapping that points to the given `path`.  We
    /// don't need this information to remove an entry from the cache, but we use it to perform
    /// consistency checks.
    pub fn delete(&self, path: &Path, file_type: fuse::FileType) {
        let mut entries = self.entries.lock().unwrap();
        if file_type == fuse::FileType::Directory {
            debug_assert!(!entries.contains_key(path), "Directories are not currently cached");
        } else {
            entries.remove(path).expect("Tried to delete unknown path from the cache");
        }
    }

    /// Renames the entry `old_path` to `new_path` in the cache.
    ///
    /// The `file_type` corresponds to the type of the mapping that points to the given `old_path`.
    /// We don't need this information to rename an entry from the cache, but we use it to perform
    /// consistency checks.
    pub fn rename(&self, old_path: &Path, new_path: PathBuf, file_type: fuse::FileType) {
        let mut entries = self.entries.lock().unwrap();
        if file_type == fuse::FileType::Directory {
            debug_assert!(!entries.contains_key(old_path), "Directories are not currently cached");
        } else {
            let node = entries.remove(old_path).expect("Tried to rename unknown path in the cache");
            entries.insert(new_path, node);
        }
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
    cache: Arc<Cache>,

    /// How long to tell the kernel to cache file metadata for.
    ttl: Timespec,
}

/// A view of a `SandboxFS` instance to allow for concurrent reconfigurations.
///
/// This structure exists because `fuse::mount` takes ownership of the file system so there is no
/// way for us to share that instance across threads.  Instead, we construct a reduced view of the
/// fields we need for reconfiguration and pass those across threads.
struct ReconfigurableSandboxFS {
    /// The root node of the file system, on which to apply all reconfiguration operations.
    root: nodes::ArcNode,

    /// Monotonically-increasing generator of identifiers for this file system instance.
    ids: Arc<IdGenerator>,

    /// Cache of sandboxfs nodes indexed by their underlying path.
    cache: Arc<Cache>,
}

/// Applies a mapping to the given root node.
///
/// This code is shared by the application of `--mapping` flags and by the application of new
/// mappings as part of a reconfiguration operation.  We want both processes to behave identically.
fn apply_mapping(mapping: &Mapping, root: &nodes::Node, ids: &IdGenerator, cache: &Cache)
    -> Fallible<()> {
    let all = mapping.path.components().collect::<Vec<_>>();
    debug_assert_eq!(Component::RootDir, all[0], "Paths in mappings are always absolute");
    let components = &all[1..];

    // The input `root` node is an existing node that corresponds to the root.  If we don't find
    // any path components in the given mapping, it means we are trying to remap that same node.
    ensure!(!components.is_empty(), "Root can be mapped at most once");

    root.map(components, &mapping.underlying_path, mapping.writable, &ids, &cache)
}

/// Creates the initial node hierarchy based on a collection of `mappings`.
fn create_root(mappings: &[Mapping], ids: &IdGenerator, cache: &Cache) -> Fallible<nodes::ArcNode> {
    let now = time::get_time();

    let (root, rest) = if mappings.is_empty() {
        (nodes::Dir::new_empty(ids.next(), None, now), mappings)
    } else {
        let first = &mappings[0];
        if first.is_root() {
            let fs_attr = fs::symlink_metadata(&first.underlying_path)
                .context(format!("Failed to map root: stat failed for {:?}",
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
            .context(format!("Cannot map '{}'", mapping))?;
    }

    Ok(root)
}

impl SandboxFS {
    /// Creates a new `SandboxFS` instance.
    fn create(mappings: &[Mapping], ttl: Timespec) -> Fallible<SandboxFS> {
        let ids = IdGenerator::new(fuse::FUSE_ROOT_ID);
        let cache = Cache::default();

        let mut nodes = HashMap::new();
        let root = create_root(mappings, &ids, &cache)?;
        assert_eq!(fuse::FUSE_ROOT_ID, root.inode());
        nodes.insert(root.inode(), root);

        Ok(SandboxFS {
            ids: Arc::from(ids),
            nodes: Arc::from(Mutex::from(nodes)),
            handles: Arc::from(Mutex::from(HashMap::new())),
            cache: Arc::from(cache),
            ttl: ttl,
        })
    }

    /// Creates a reconfigurable view of this file system, to safely pass across threads.
    fn reconfigurable(&mut self) -> ReconfigurableSandboxFS {
        ReconfigurableSandboxFS {
            root: self.find_node(fuse::FUSE_ROOT_ID),
            ids: self.ids.clone(),
            cache: self.cache.clone(),
        }
    }

    /// Gets a node given its `inode`.
    ///
    /// We assume that the inode number is valid and that we have a known node for it; otherwise,
    /// we crash.  The rationale for this is that this function is always called on inode numbers
    /// requested by the kernel, and we can trust that the kernel will only ever ask us for inode
    /// numbers we have previously told it about.
    fn find_node(&mut self, inode: u64) -> nodes::ArcNode {
        let nodes = self.nodes.lock().unwrap();
        nodes.get(&inode).expect("Kernel requested unknown inode").clone()
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

    /// Opens a new handle for the given `inode`.
    ///
    /// This is a helper function to implement the symmetric `open` and `opendir` hooks.
    fn open_common(&mut self, inode: u64, flags: u32, reply: fuse::ReplyOpen) {
        let node = self.find_node(inode);

        match node.open(flags) {
            Ok(handle) => {
                let fh = self.insert_handle(handle);
                reply.opened(fh, 0);
            },
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    /// Releases the given file handle `fh`.
    ///
    /// This is a helper function to implement the symmetric `release` and `releasedir` hooks.
    fn release_common(&mut self, fh: u64, reply: fuse::ReplyEmpty) {
        {
            let mut handles = self.handles.lock().unwrap();
            handles.remove(&fh).expect("Kernel tried to release an unknown handle");
        }
        reply.ok();
    }
}

impl fuse::Filesystem for SandboxFS {
    fn create(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, mode: u32, flags: u32,
        reply: fuse::ReplyCreate) {
        let dir_node = self.find_node(parent);
        if !dir_node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        match dir_node.create(name, mode, flags, &self.ids, &self.cache) {
            Ok((node, handle, attr)) => {
                self.insert_node(node);
                let fh = self.insert_handle(handle);
                reply.created(&self.ttl, &attr, IdGenerator::GENERATION, fh, 0);
            },
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn getattr(&mut self, _req: &fuse::Request, inode: u64, reply: fuse::ReplyAttr) {
        let node = self.find_node(inode);
        match node.getattr() {
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
        let dir_node = self.find_node(parent);
        match dir_node.lookup(name, &self.ids, &self.cache) {
            Ok((node, attr)) => {
                {
                    let mut nodes = self.nodes.lock().unwrap();
                    if !nodes.contains_key(&node.inode()) {
                        nodes.insert(node.inode(), node);
                    }
                }
                reply.entry(&self.ttl, &attr, IdGenerator::GENERATION);
            },
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn mkdir(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, mode: u32,
        reply: fuse::ReplyEntry) {
        let dir_node = self.find_node(parent);
        if !dir_node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        match dir_node.mkdir(name, mode, &self.ids, &self.cache) {
            Ok((node, attr)) => {
                self.insert_node(node);
                reply.entry(&self.ttl, &attr, IdGenerator::GENERATION);
            },
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn mknod(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, mode: u32, rdev: u32,
        reply: fuse::ReplyEntry) {
        let dir_node = self.find_node(parent);
        if !dir_node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        match dir_node.mknod(name, mode, rdev, &self.ids, &self.cache) {
            Ok((node, attr)) => {
                self.insert_node(node);
                reply.entry(&self.ttl, &attr, IdGenerator::GENERATION);
            },
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn open(&mut self, _req: &fuse::Request, inode: u64, flags: u32, reply: fuse::ReplyOpen) {
        self.open_common(inode, flags, reply)
    }

    fn opendir(&mut self, _req: &fuse::Request, inode: u64, flags: u32, reply: fuse::ReplyOpen) {
        self.open_common(inode, flags, reply)
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
        match handle.readdir(&self.ids, &self.cache, offset, &mut reply) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn readlink(&mut self, _req: &fuse::Request, inode: u64, reply: fuse::ReplyData) {
        let node = self.find_node(inode);
        match node.readlink() {
            Ok(target) => reply.data(target.as_os_str().as_bytes()),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn release(&mut self, _req: &fuse::Request, _inode: u64, fh: u64, _flags: u32, _lock_owner: u64,
        _flush: bool, reply: fuse::ReplyEmpty) {
        self.release_common(fh, reply)
    }

    fn releasedir(&mut self, _req: &fuse::Request, _inode: u64, fh: u64, _flags: u32,
        reply: fuse::ReplyEmpty) {
        self.release_common(fh, reply)
    }

    fn rename(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, new_parent: u64,
        new_name: &OsStr, reply: fuse::ReplyEmpty) {
        let dir_node = self.find_node(parent);
        if !dir_node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        let result = if parent == new_parent {
            dir_node.rename(name, new_name, &self.cache)
        } else {
            let new_dir_node = self.find_node(new_parent);
            if !new_dir_node.writable() {
                reply.error(Errno::EPERM as i32);
                return;
            }
            dir_node.rename_and_move_source(name, new_dir_node, new_name, &self.cache)
        };
        match result {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn rmdir(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, reply: fuse::ReplyEmpty) {
        let dir_node = self.find_node(parent);
        if !dir_node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        match dir_node.rmdir(name, &self.cache) {
            Ok(_) => reply.ok(),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn setattr(&mut self, _req: &fuse::Request, inode: u64, mode: Option<u32>, uid: Option<u32>,
        gid: Option<u32>, size: Option<u64>, atime: Option<Timespec>, mtime: Option<Timespec>,
        _fh: Option<u64>, _crtime: Option<Timespec>, _chgtime: Option<Timespec>,
        _bkuptime: Option<Timespec>, _flags: Option<u32>, reply: fuse::ReplyAttr) {
        let node = self.find_node(inode);
        if !node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        let values = nodes::AttrDelta {
            mode: mode.map(|m| sys::stat::Mode::from_bits_truncate(m as sys::stat::mode_t)),
            uid: uid.map(unistd::Uid::from_raw),
            gid: gid.map(unistd::Gid::from_raw),
            atime: atime.map(nodes::conv::timespec_to_timeval),
            mtime: mtime.map(nodes::conv::timespec_to_timeval),
            size: size,
        };
        match node.setattr(&values) {
            Ok(attr) => reply.attr(&self.ttl, &attr),
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn symlink(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, link: &Path,
        reply: fuse::ReplyEntry) {
        let dir_node = self.find_node(parent);
        if !dir_node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        match dir_node.symlink(name, link, &self.ids, &self.cache) {
            Ok((node, attr)) => {
                self.insert_node(node);
                reply.entry(&self.ttl, &attr, IdGenerator::GENERATION);
            },
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn unlink(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, reply: fuse::ReplyEmpty) {
        let dir_node = self.find_node(parent);
        if !dir_node.writable() {
            reply.error(Errno::EPERM as i32);
            return;
        }

        match dir_node.unlink(name, &self.cache) {
            Ok(_) => reply.ok(),
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
}

impl reconfig::ReconfigurableFS for ReconfigurableSandboxFS {
    fn map(&self, mapping: &Mapping) -> Fallible<()> {
        apply_mapping(mapping, self.root.as_ref(), self.ids.as_ref(), self.cache.as_ref())
            .context(format!("Cannot map '{}'", mapping))?;
        Ok(())
    }

    fn unmap<P: AsRef<Path>>(&self, path: P) -> Fallible<()> {
        let all = path.as_ref().components().collect::<Vec<_>>();
        debug_assert_eq!(Component::RootDir, all[0], "Paths to unmap are always absolute");
        let components = &all[1..];

        ensure!(!components.is_empty(), "Root cannot be unmapped");

        self.root.unmap(components)
    }
}

/// Mounts a new sandboxfs instance on the given `mount_point` and maps all `mappings` within it.
pub fn mount(mount_point: &Path, options: &[&str], mappings: &[Mapping], ttl: Timespec,
    input: fs::File, output: fs::File) -> Fallible<()> {
    let mut os_options = options.iter().map(|o| o.as_ref()).collect::<Vec<&OsStr>>();

    // Delegate permissions checks to the kernel for efficiency and to avoid having to implement
    // them on our own.
    os_options.push(OsStr::new("-o"));
    os_options.push(OsStr::new("default_permissions"));

    let mut fs = SandboxFS::create(mappings, ttl)?;
    let reconfigurable_fs = fs.reconfigurable();
    info!("Mounting file system onto {:?}", mount_point);

    let (signals, mut session) = {
        let installer = concurrent::SignalsInstaller::prepare();
        let mut session = fuse::Session::new(fs, &mount_point, &os_options)?;
        let signals = installer.install(PathBuf::from(mount_point))?;
        (signals, session)
    };

    let config_handler = {
        let mut input = concurrent::ShareableFile::from(input);
        let reader = io::BufReader::new(input.reader()?);
        let writer = io::BufWriter::new(output);
        let handler = thread::spawn(move || {
            reconfig::run_loop(reader, writer, &reconfigurable_fs);
        });

        session.run()?;
        handler
    };
    // The input must be closed to let the reconfiguration thread to exit, which then lets the join
    // operation below complete, hence the scope above.

    if let Some(signo) = signals.caught() {
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
    use tempfile::tempdir;

    #[test]
    fn flatten_causes_one() {
        let err = Error::from(format_err!("root cause"));
        assert_eq!("root cause", flatten_causes(&err));
    }

    #[test]
    fn flatten_causes_multiple() {
        let err = Error::from(format_err!("root cause"));
        let err = Error::from(err.context("intermediate"));
        let err = Error::from(err.context("top"));
        assert_eq!("top: intermediate: root cause", flatten_causes(&err));
    }

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
            PathBuf::from("/a/b"), irrelevant.clone(), false).unwrap().is_root());
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
    fn cache_behavior() {
        let root = tempdir().unwrap();

        let dir1 = root.path().join("dir1");
        fs::create_dir(&dir1).unwrap();
        let dir1attr = fs::symlink_metadata(&dir1).unwrap();

        let file1 = root.path().join("file1");
        drop(fs::File::create(&file1).unwrap());
        let file1attr = fs::symlink_metadata(&file1).unwrap();

        let file2 = root.path().join("file2");
        drop(fs::File::create(&file2).unwrap());
        let file2attr = fs::symlink_metadata(&file2).unwrap();

        let ids = IdGenerator::new(1);
        let cache = Cache::default();

        // Directories are not cached no matter what.
        assert_eq!(1, cache.get_or_create(&ids, &dir1, &dir1attr, false).inode());
        assert_eq!(2, cache.get_or_create(&ids, &dir1, &dir1attr, false).inode());
        assert_eq!(3, cache.get_or_create(&ids, &dir1, &dir1attr, true).inode());

        // Different files get different nodes.
        assert_eq!(4, cache.get_or_create(&ids, &file1, &file1attr, false).inode());
        assert_eq!(5, cache.get_or_create(&ids, &file2, &file2attr, true).inode());

        // Files we queried before but with different writability get different nodes.
        assert_eq!(6, cache.get_or_create(&ids, &file1, &file1attr, true).inode());
        assert_eq!(7, cache.get_or_create(&ids, &file2, &file2attr, false).inode());

        // We get cache hits when everything matches previous queries.
        assert_eq!(6, cache.get_or_create(&ids, &file1, &file1attr, true).inode());
        assert_eq!(7, cache.get_or_create(&ids, &file2, &file2attr, false).inode());

        // We don't get cache hits for nodes whose writability changed.
        assert_eq!(8, cache.get_or_create(&ids, &file1, &file1attr, false).inode());
        assert_eq!(9, cache.get_or_create(&ids, &file2, &file2attr, true).inode());
    }

    #[test]
    fn cache_nodes_support_all_file_types() {
        let ids = IdGenerator::new(1);
        let cache = Cache::default();

        for (_fuse_type, path) in testutils::AllFileTypes::new().entries {
            let fs_attr = fs::symlink_metadata(&path).unwrap();
            // The following panics if it's impossible to represent the given file type, which is
            // what we are testing.
            cache.get_or_create(&ids, &path, &fs_attr, false);
        }
    }
}
