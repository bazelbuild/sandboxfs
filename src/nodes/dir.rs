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

use {create_as, IdGenerator};
use failure::{Fallible, ResultExt};
use nix::{errno, fcntl, sys, unistd};
use nix::dir as rawdir;
use nodes::{
    ArcHandle, ArcNode, AttrDelta, Cache, Handle, KernelError, Node, NodeResult, conv, setattr};
use std::collections::HashMap;
use std::ffi::{OsStr, OsString};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{self as unix_fs, DirBuilderExt, OpenOptionsExt};
use std::fs;
use std::io;
use std::path::{Component, Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Takes the components of a path and returns the first normal component and the rest.
///
/// This assumes that the input path is normalized and that the very first component is a normal
/// component as defined by `Component::Normal`.
fn split_components<'a>(components: &'a [Component<'a>]) -> (&'a OsStr, &'a [Component<'a>]) {
    debug_assert!(!components.is_empty());
    let name = match components[0] {
        Component::Normal(name) => name,
        _ => panic!("Input list of components is not normalized"),
    };
    (name, &components[1..])
}

/// Contents of a single `fuse::ReplyDirectory` reply; used for pagination.
struct ReplyEntry {
    inode: u64,
    fs_type: fuse::FileType,
    name: OsString,
}

/// Handle for an open directory.
struct OpenDir {
    // These are copies of the fields that also exist in the Dir corresponding to this OpenDir.
    // Ideally we could just hold an immutable reference to the Dir instance... but this is hard
    // to do because, when opendir() gets called on an abstract Node, we do not get access to the
    // ArcNode that corresponds to it (to clone it).  Given that these values are immutable on
    // the node, holding a copy here is fine.
    inode: u64,
    writable: bool,
    state: Arc<Mutex<MutableDir>>,

    /// Handle for the open directory file descriptor.  This is `None` if the directory does not
    /// have an underlying path.
    handle: Mutex<Option<rawdir::Dir>>,

    /// Contents of this directory.  This is populated on the first `readdir` request that has an
    /// offset of zero and reused for all further calls until the contents are consumed.
    ///
    /// Doing this means that mutations to the directory won't be visible to `readdir` while a
    /// "stream" of partial `readdir` calls is in progress.  This is probably a good thing.
    reply_contents: Mutex<Vec<ReplyEntry>>,
}

impl OpenDir {
    /// Reads all directory entries in one go.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when readdir discovers an underlying node that was not yet known.
    fn readdirall(&self, ids: &IdGenerator, cache: &dyn Cache) -> NodeResult<Vec<ReplyEntry>> {
        let mut reply = vec!();

        let mut state = self.state.lock().unwrap();

        reply.push(ReplyEntry {
            inode: self.inode,
            fs_type: fuse::FileType::Directory,
            name: OsString::from(".")
        });
        reply.push(ReplyEntry {
            inode: state.parent,
            fs_type: fuse::FileType::Directory,
            name: OsString::from("..")
        });

        // First, return the entries that correspond to explicit mappings performed by the user at
        // either mount time or during a reconfiguration.  Those should clobber any on-disk
        // contents that we discover later when we issue the readdir on the underlying directory,
        // if any.
        for (name, dirent) in &state.children {
            if dirent.explicit_mapping {
                reply.push(ReplyEntry {
                    inode: dirent.node.inode(),
                    fs_type: dirent.node.file_type_cached(),
                    name: name.clone()
                });
            }
        }

        let mut handle = self.handle.lock().unwrap();

        if handle.is_none() {
            debug_assert!(state.underlying_path.is_none());
            return Ok(reply);
        }
        debug_assert!(state.underlying_path.is_some());
        let handle = handle.as_mut().unwrap();
        for entry in handle.iter() {
            let entry = entry?;
            let name = entry.file_name();

            let name = OsStr::from_bytes(name.to_bytes()).to_os_string();

            if name == "." || name == ".." {
                continue;
            }

            if let Some(dirent) = state.children.get(&name) {
                // Found a previously-known on-disk entry.  Must return it "as is" (even if its
                // type might have changed) because, if we called into `cache.get_or_create` below,
                // we might recreate the node unintentionally.  Note that mappings were handled
                // earlier, so only handle the non-mapping case here.
                if !dirent.explicit_mapping {
                    reply.push(ReplyEntry {
                        inode: dirent.node.inode(),
                        fs_type: dirent.node.file_type_cached(),
                        name: name.clone(),
                    });
                }
                continue;
            }

            let path = state.underlying_path.as_ref().unwrap().join(&name);

            // TODO(jmmv): In theory we shouldn't need to issue a stat for every entry during a
            // readdir.  However, it's much easier to handle things this way because we currently
            // require a file's metadata in order to instantiate a node.  Note that the Go variant
            // of this code does the same and an attempt to "fix" this resulted in more complex
            // code and no visible performance gains.  That said, it'd be worth to investigate this
            // again.
            let fs_attr = fs::symlink_metadata(&path)?;

            let fs_type = conv::filetype_fs_to_fuse(&path, fs_attr.file_type());
            let child = cache.get_or_create(ids, &path, &fs_attr, self.writable);

            reply.push(ReplyEntry { inode: child.inode(), fs_type: fs_type, name: name.clone() });

            // Do the insertion into state.children after calling reply.add() to be able to move
            // the name into the key without having to copy it again.
            let dirent = Dirent {
                node: child.clone(),
                explicit_mapping: false,
            };
            // TODO(jmmv): We should remove stale entries at some point (possibly here), but the Go
            // variant does not do this so any implications of this are not tested.  The reason this
            // hasn't caused trouble yet is because: on readdir, we don't use any contents from
            // state.children that correspond to unmapped entries, and any stale entries visited
            // during lookup will result in an ENOENT.
            state.children.insert(name, dirent);
        }
        // No need to worry about rewinding handle.iter() for future reads on the same OpenDir:
        // the rawdir::Dir implementation does this for us.
        Ok(reply)
    }
}

impl Handle for OpenDir {
    fn readdir(&self, ids: &IdGenerator, cache: &dyn Cache, offset: i64,
        reply: &mut fuse::ReplyDirectory) -> NodeResult<()> {
        let mut offset: usize = offset as usize;

        let mut contents = self.reply_contents.lock().unwrap();
        if offset == 0 {
            *contents = self.readdirall(ids, cache)?;
        } else {
            // When the kernel asks us to return extra entries from a partially-read directory, it
            // does so by giving us the offset of the last entry we returned -- not the first one
            // that we ought to return.  Therefore, advance the offset by one to avoid duplicate
            // return values and to avoid entering an infinite loop.
            offset += 1;
        }

        while offset < contents.len() {
            let entry = &contents[offset];
            if reply.add(entry.inode, offset as i64, entry.fs_type, &entry.name) {
                break;  // Reply buffer is full.
            }
            offset += 1;
        }
        Ok(())
    }
}

/// Representation of a directory entry.
#[derive(Clone)]
pub struct Dirent {
    node: ArcNode,
    explicit_mapping: bool,
}

/// Representation of a directory node.
pub struct Dir {
    inode: u64,
    writable: bool,
    state: Arc<Mutex<MutableDir>>,
}

/// Holds the mutable data of a directory node.
pub struct MutableDir {
    parent: u64,
    underlying_path: Option<PathBuf>,
    attr: fuse::FileAttr,
    children: HashMap<OsString, Dirent>,
}

impl Dir {
    /// Creates a new scaffold directory to represent an in-memory directory.
    ///
    /// The directory's timestamps are set to `now` and the ownership is set to the current user.
    pub fn new_empty(inode: u64, parent: Option<&dyn Node>, now: time::Timespec) -> ArcNode {
        let attr = fuse::FileAttr {
            ino: inode,
            kind: fuse::FileType::Directory,
            nlink: 2,  // "." entry plus whichever initial named node points at this.
            size: 2,  // TODO(jmmv): Reevaluate what directory sizes should be.
            blocks: 1,  // TODO(jmmv): Reevaluate what directory blocks should be.
            atime: now,
            mtime: now,
            ctime: now,
            crtime: now,
            perm: 0o555 as u16,  // Scaffold directories cannot be mutated by the user.
            uid: unistd::getuid().as_raw(),
            gid: unistd::getgid().as_raw(),
            rdev: 0,
            flags: 0,
        };

        let state = MutableDir {
            parent: parent.map_or(inode, Node::inode),
            underlying_path: None,
            attr: attr,
            children: HashMap::new(),
        };

        Arc::new(Dir {
            inode: inode,
            writable: false,
            state: Arc::from(Mutex::from(state)),
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
        -> ArcNode {
        if !fs_attr.is_dir() {
            panic!("Can only construct based on dirs");
        }

        // For directories, assume a fixed link count of 2 that does not change throughout the
        // lifetime of the directory (except for its own removal).
        //
        // An alternative would be to inherit the link count from the underlying file system and
        // trust that it is accurate in the (typical) absence of hard links for directories.
        // I tried that on 2020-02-10 on macOS Catalina with APFS and discovered that this
        // assumption does not work because this system seems to count *all* directory entries as
        // links (not just subdirectories).
        let nlink = 2;

        let attr = conv::attr_fs_to_fuse(underlying_path, inode, nlink, &fs_attr);

        let state = MutableDir {
            parent: inode,
            underlying_path: Some(PathBuf::from(underlying_path)),
            attr: attr,
            children: HashMap::new(),
        };

        Arc::new(Dir { inode, writable, state: Arc::from(Mutex::from(state)) })
    }

    /// Creates a new scaffold directory as a child of the current one.
    ///
    /// Errors are all logged, not reported.  The rationale is that a scaffold directory for an
    /// intermediate path component of a mapping has to always be created, as it takes preference
    /// over any other on-disk contents.
    ///
    /// This is purely a helper function for `map`.  As a result, the caller is responsible for
    /// inserting the new directory into the children of the current directory.
    fn new_scaffold_child(&self, underlying_path: Option<&PathBuf>, name: &OsStr, ids: &IdGenerator,
        now: time::Timespec) -> ArcNode {
        if let Some(path) = underlying_path {
            let child_path = path.join(name);
            match fs::symlink_metadata(&child_path) {
                Ok(fs_attr) => {
                    if fs_attr.is_dir() {
                        return Dir::new_mapped(ids.next(), &child_path, &fs_attr, self.writable);
                    }

                    info!("Mapping clobbers non-directory {} with an immutable directory",
                        child_path.display());
                },
                Err(e) => {
                    if e.kind() != io::ErrorKind::NotFound {
                        warn!("Mapping clobbers {} due to an error: {}", child_path.display(), e);
                    }
                },
            }
        }
        Dir::new_empty(ids.next(), Some(self), now)
    }

    /// Same as `getattr` but with the node already locked.
    fn getattr_locked(inode: u64, state: &mut MutableDir) -> NodeResult<fuse::FileAttr> {
        if let Some(path) = &state.underlying_path {
            let fs_attr = fs::symlink_metadata(path)?;
            if !fs_attr.is_dir() {
                warn!("Path {} backing a directory node is no longer a directory; got {:?}",
                    path.display(), fs_attr.file_type());
                return Err(KernelError::from_errno(errno::Errno::EIO));
            }
            state.attr = conv::attr_fs_to_fuse(path, inode, state.attr.nlink, &fs_attr);
        }

        Ok(state.attr)
    }

    /// Gets the underlying path of the entry `name` in this directory.
    ///
    /// This also ensures that the entry is writable, which is determined by the directory itself
    /// being mapped to an underlying path and the entry not being an explicit mapping.
    fn get_writable_path(state: &mut MutableDir, name: &OsStr) -> NodeResult<PathBuf> {
        if state.underlying_path.is_none() {
            return Err(KernelError::from_errno(errno::Errno::EPERM));
        }
        let path = state.underlying_path.as_ref().unwrap().join(name);

        if let Some(node) = state.children.get(name) {
            if node.explicit_mapping {
                return Err(KernelError::from_errno(errno::Errno::EPERM));
            }
        };

        Ok(path)
    }

    // Same as `lookup` but with the node already locked.
    fn lookup_locked(writable: bool, state: &mut MutableDir, name: &OsStr, ids: &IdGenerator,
        cache: &dyn Cache) -> NodeResult<(ArcNode, fuse::FileAttr)> {
        if let Some(dirent) = state.children.get(name) {
            let refreshed_attr = dirent.node.getattr()?;
            return Ok((dirent.node.clone(), refreshed_attr))
        }

        let (child, attr) = {
            let path = match &state.underlying_path {
                Some(underlying_path) => underlying_path.join(name),
                None => return Err(KernelError::from_errno(errno::Errno::ENOENT)),
            };
            let fs_attr = fs::symlink_metadata(&path)?;
            let node = cache.get_or_create(ids, &path, &fs_attr, writable);
            let attr = conv::attr_fs_to_fuse(
                path.as_path(), node.inode(), node.getattr()?.nlink, &fs_attr);
            (node, attr)
        };
        let dirent = Dirent {
            node: child.clone(),
            explicit_mapping: false,
        };
        state.children.insert(name.to_os_string(), dirent);
        Ok((child, attr))
    }

    /// Obtains the node and attributes of an underlying file immediately after its creation.
    ///
    /// `writable` and `state` are the properties of the node, passed in as arguments because we
    /// have to hold the node locked already.
    ///
    /// `path` and `name` are the path to the underlying file and the basename to lookup in the
    /// directory, respectively.  It is expected that the basename of `path` matches `name`.
    ///
    /// `exp_type` is the type of the node we expect to find for the just-created file.  If the
    /// node doesn't match this type, it means we encountered a race on the underlying file system
    /// and we fail the lookup.  (This is an artifact of how we currently implement this function
    /// as this condition should just be impossible.)
    fn post_create_lookup(writable: bool, state: &mut MutableDir, path: &Path, name: &OsStr,
        exp_type: fuse::FileType, ids: &IdGenerator, cache: &dyn Cache)
        -> NodeResult<(ArcNode, fuse::FileAttr)> {
        debug_assert_eq!(path.file_name().unwrap(), name);

        // TODO(https://github.com/bazelbuild/sandboxfs/issues/43): We abuse lookup here to handle
        // the node creation and the child insertion into the directory, but we shouldn't do this
        // because lookup performs an extra stat that we should not be issuing.  But to resolve this
        // we need to be able to synthesize the returned attr, which means we need to track ctimes
        // internally.
        match Dir::lookup_locked(writable, state, name, ids, cache) {
            Ok((node, attr)) => {
                if node.file_type_cached() != exp_type {
                    warn!("Newly-created file {} was replaced or deleted before create finished",
                        path.display());
                    return Err(KernelError::from_errno(errno::Errno::EIO));
                }
                Ok((node, attr))
            },
            Err(e) => {
                if let Err(e) = fs::remove_file(&path) {
                    warn!("Failed to clean up newly-created {}: {}", path.display(), e);
                }
                Err(e)
            }
        }
    }

    /// Common implementation for the `rmdir` and `unlink` operations.
    ///
    /// The behavior of these operations differs only in the syscall we invoke to delete the
    /// underlying entry, which is passed in as the `remove` parameter.
    fn remove_any<R>(&self, name: &OsStr, remove: R, cache: &dyn Cache) -> NodeResult<()>
        where R: Fn(&PathBuf) -> io::Result<()> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        remove(&path)?;

        // Removing the underlying path from the cache is not racy within the same directory: we
        // hold the directory node locked while we perform the operations below to remove the child,
        // which means that no other lookup on the node can complete.
        //
        // However... this is racy if the same underlying path is mapped in more than one location
        // because different lookups on different nodes could still race against the cache state.
        // We don't bother for now though: the Rust FUSE library serializes all requests so this
        // situation cannot arise.
        let entry = state.children.remove(name)
            .expect("Presence guaranteed by get_writable_path call above");
        entry.node.delete(cache);
        Ok(())
    }
}

impl Node for Dir {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn file_type_cached(&self) -> fuse::FileType {
        fuse::FileType::Directory
    }

    fn delete(&self, cache: &dyn Cache) {
        let mut state = self.state.lock().unwrap();
        assert!(
            state.underlying_path.is_some(),
            "Delete already called or trying to delete an explicit mapping");
        cache.delete(state.underlying_path.as_ref().unwrap(), state.attr.kind);
        state.underlying_path = None;
        // Make the hard link count for the directory be zero.  This is pretty much arbitrary as the
        // semantics for hard link counts on directories are not well defined, and thus different
        // OSes and file systems behave inconsistently.  For example, Linux's FUSE forces this to
        // zero, and macOS's APFS keeps this at 2.
        debug_assert!(state.attr.nlink >= 2);
        state.attr.nlink -= 2;
    }

    fn set_underlying_path(&self, path: &Path, cache: &dyn Cache) {
        let mut state = self.state.lock().unwrap();
        debug_assert!(state.underlying_path.is_some(),
            "Renames should not have been allowed in scaffold or deleted nodes");
        cache.rename(
            state.underlying_path.as_ref().unwrap(), path.to_owned(), state.attr.kind);
        state.underlying_path = Some(PathBuf::from(path));

        // This is racy: if other file operations are going on inside this subtree, they will fail
        // with ENOENT until we have updated their underlying paths after the move.  However, as we
        // are currently single-threaded (because the Rust FUSE bindings don't support multiple
        // threads), we are fine.
        for (name, dirent) in &state.children {
            dirent.node.set_underlying_path(&path.join(name), cache);
        }
    }

    fn find_path(&self, components: &[Component]) -> Fallible<ArcNode> {
        debug_assert!(
            !components.is_empty(),
            "Must not be reached because we don't have the containing ArcNode to return it");
        let (name, remainder) = split_components(components);

        let state = self.state.lock().unwrap();

        match state.children.get(name) {
            Some(dirent) => {
                if remainder.is_empty() {
                    Ok(dirent.node.clone())
                } else {
                    ensure!(dirent.explicit_mapping, "Not a mapping");
                    ensure!(
                        dirent.node.file_type_cached() == fuse::FileType::Directory,
                        "Not a directory");
                    dirent.node.find_path(remainder)
                }
            },
            None => Err(format_err!("Not mapped")),
        }
    }

    fn find_or_create_path(&self, components: &[Component], ids: &IdGenerator, cache: &dyn Cache)
        -> Fallible<ArcNode> {
        debug_assert!(
            !components.is_empty(),
            "Must not be reached because we don't have the containing ArcNode to return it");
        let (name, remainder) = split_components(components);

        let mut state = self.state.lock().unwrap();

        if let Some(dirent) = state.children.get(name) {
            if remainder.is_empty() {
                ensure!(dirent.explicit_mapping, "Not a mapping");
                return Ok(dirent.node.clone());
            } else {
                // TODO(jmmv): We should probably mark this dirent as an explicit mapping if it
                // already wasn't.
                ensure!(
                    dirent.node.file_type_cached() == fuse::FileType::Directory,
                    "Already mapped as a non-directory");
                return dirent.node.find_or_create_path(remainder, ids, cache);
            }
        }

        let child = self.new_scaffold_child(None, name, ids, time::get_time());

        let dirent = Dirent { node: child.clone(), explicit_mapping: true };
        state.children.insert(name.to_os_string(), dirent);

        if remainder.is_empty() {
            Ok(child)
        } else {
            ensure!(child.file_type_cached() == fuse::FileType::Directory, "Already mapped");
            child.find_or_create_path(remainder, ids, cache)
        }
    }

    fn map(&self, components: &[Component], underlying_path: &Path, writable: bool,
        ids: &IdGenerator, cache: &dyn Cache) -> Fallible<ArcNode> {
        debug_assert!(
            !components.is_empty(),
            "Must not be reached because we don't have the containing ArcNode to return it");
        let (name, remainder) = split_components(components);

        let mut state = self.state.lock().unwrap();

        if let Some(dirent) = state.children.get(name) {
            // TODO(jmmv): We should probably mark this dirent as an explicit mapping if it already
            // wasn't, but the Go variant of this code doesn't do this -- so investigate later.
            ensure!(dirent.node.file_type_cached() == fuse::FileType::Directory
                && !remainder.is_empty(), "Already mapped");
            return dirent.node.map(remainder, underlying_path, writable, ids, cache);
        }

        let child = if remainder.is_empty() {
            let fs_attr = fs::symlink_metadata(underlying_path)
                .context(format!("Stat failed for {:?}", underlying_path))?;
            cache.get_or_create(ids, underlying_path, &fs_attr, writable)
        } else {
            self.new_scaffold_child(state.underlying_path.as_ref(), name, ids, time::get_time())
        };

        let dirent = Dirent { node: child.clone(), explicit_mapping: true };
        state.children.insert(name.to_os_string(), dirent);

        if remainder.is_empty() {
            Ok(child)
        } else {
            ensure!(child.file_type_cached() == fuse::FileType::Directory, "Already mapped");
            child.map(remainder, underlying_path, writable, ids, cache)
        }
    }

    fn unmap(&self, components: &[Component]) -> Fallible<()> {
        let (name, remainder) = split_components(components);

        let mut state = self.state.lock().unwrap();

        if remainder.is_empty() {
            match state.children.remove_entry(name) {
                Some((name, dirent)) => {
                    if dirent.explicit_mapping {
                        // TODO(jmmv): Invalidate kernel dirent once the FUSE crate supports it.
                        Ok(())
                    } else {
                        let err = format_err!("{:?} is not a mapping", &name);
                        state.children.insert(name, dirent);
                        Err(err)
                    }
                },
                None => Err(format_err!("Unknown entry")),
            }
        } else {
            match state.children.get(name) {
                Some(dirent) => dirent.node.unmap(remainder),
                None => Err(format_err!("Unknown component in entry")),
            }
        }
    }

    #[allow(clippy::type_complexity)]
    fn create(&self, name: &OsStr, uid: unistd::Uid, gid: unistd::Gid, mode: u32, flags: u32,
        ids: &IdGenerator, cache: &dyn Cache) -> NodeResult<(ArcNode, ArcHandle, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        let mut options = conv::flags_to_openoptions(flags, self.writable)?;
        options.create(true);
        options.mode(mode);

        let file = create_as(&path, uid, gid, |p| options.open(&p), |p| fs::remove_file(&p))?;
        let (node, attr) = Dir::post_create_lookup(self.writable, &mut state, &path, name,
            fuse::FileType::RegularFile, ids, cache)?;
        Ok((node.clone(), node.handle_from(file), attr))
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        Dir::getattr_locked(self.inode, &mut state)
    }

    fn getxattr(&self, name: &OsStr) -> NodeResult<Option<Vec<u8>>> {
        let state = self.state.lock().unwrap();
        match &state.underlying_path {
            Some(path) => Ok(xattr::get(path, name)?),
            None => Ok(None),
        }
    }

    fn listxattr(&self) -> NodeResult<Option<xattr::XAttrs>> {
        let state = self.state.lock().unwrap();
        match &state.underlying_path {
            Some(path) => Ok(Some(xattr::list(path)?)),
            None => Ok(None),
        }
    }

    fn lookup(&self, name: &OsStr, ids: &IdGenerator, cache: &dyn Cache)
        -> NodeResult<(ArcNode, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        Dir::lookup_locked(self.writable, &mut state, name, ids, cache)
    }

    fn mkdir(&self, name: &OsStr, uid: unistd::Uid, gid: unistd::Gid, mode: u32, ids: &IdGenerator,
        cache: &dyn Cache) -> NodeResult<(ArcNode, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        create_as(
            &path, uid, gid,
            |p| fs::DirBuilder::new().mode(mode).create(&p),
            |p| fs::remove_dir(&p))?;
        Dir::post_create_lookup(self.writable, &mut state, &path, name,
            fuse::FileType::Directory, ids, cache)
    }

    fn mknod(&self, name: &OsStr, uid: unistd::Uid, gid: unistd::Gid, mode: u32, rdev: u32,
        ids: &IdGenerator, cache: &dyn Cache) -> NodeResult<(ArcNode, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        if mode > u32::from(std::u16::MAX) {
            warn!("mknod got too-big mode {} (exceeds {})", mode, std::u16::MAX);
        }
        let mode = mode as sys::stat::mode_t;

        // We have to break apart the incoming mode into a separate file type flag and a permissions
        // set... only to have mknod() combine them later once again.  Doesn't make a lot of sense
        // but that the API we get from nix, hence ensure we are doing the right thing.
        let (sflag, perm) = {
            let sflag = sys::stat::SFlag::from_bits_truncate(mode);
            let perm = sys::stat::Mode::from_bits_truncate(mode);

            let truncated_mode = sflag.bits() | perm.bits();
            if truncated_mode != mode {
                warn!("mknod cannot only handle {} from mode {}", truncated_mode, mode);
            }

            (sflag, perm)
        };

        let exp_filetype = match sflag {
            sys::stat::SFlag::S_IFBLK => fuse::FileType::BlockDevice,
            sys::stat::SFlag::S_IFCHR => fuse::FileType::CharDevice,
            sys::stat::SFlag::S_IFIFO => fuse::FileType::NamedPipe,
            sys::stat::SFlag::S_IFREG => fuse::FileType::RegularFile,
            _ => {
                warn!("mknod received request to create {} with type {:?}, which is not supported",
                    path.display(), sflag);
                return Err(KernelError::from_errno(errno::Errno::EIO));
            },
        };

        #[allow(clippy::cast_lossless)]
        create_as(
            &path, uid, gid,
            |p| sys::stat::mknod(p, sflag, perm, rdev as sys::stat::dev_t),
            |p| unistd::unlink(p))?;
        Dir::post_create_lookup(self.writable, &mut state, &path, name, exp_filetype, ids, cache)
    }

    fn open(&self, flags: u32) -> NodeResult<ArcHandle> {
        let flags = flags as i32;
        let oflag = fcntl::OFlag::from_bits_truncate(flags);

        let handle = {
            let state = self.state.lock().unwrap();

            match state.underlying_path.as_ref() {
                Some(path) => Some(rawdir::Dir::open(path, oflag, sys::stat::Mode::S_IRUSR)?),
                None => None,
            }
        };

        Ok(Arc::from(OpenDir {
            inode: self.inode,
            writable: self.writable,
            state: self.state.clone(),
            handle: Mutex::from(handle),
            reply_contents: Mutex::from(vec!()),
        }))
    }

    fn removexattr(&self, name: &OsStr) -> NodeResult<()> {
        let state = self.state.lock().unwrap();
        match &state.underlying_path {
            Some(path) => Ok(xattr::remove(path, name)?),
            None => Err(KernelError::from_errno(errno::Errno::EACCES)),
        }
    }

    fn rename(&self, old_name: &OsStr, new_name: &OsStr, cache: &dyn Cache) -> NodeResult<()> {
        let mut state = self.state.lock().unwrap();

        let old_path = Dir::get_writable_path(&mut state, old_name)?;
        let new_path = Dir::get_writable_path(&mut state, new_name)?;

        fs::rename(&old_path, &new_path)?;

        let dirent = state.children.remove(old_name)
            .expect("get_writable_path call above ensured the child exists");
        dirent.node.set_underlying_path(&new_path, cache);
        state.children.insert(new_name.to_owned(), dirent);
        Ok(())
    }

    fn rename_and_move_source(&self, old_name: &OsStr, new_dir: ArcNode, new_name: &OsStr,
        cache: &dyn Cache) -> NodeResult<()> {
        debug_assert!(self.inode != new_dir.as_ref().inode(),
            "Same-directory renames have to be done via `rename`");

        let mut state = self.state.lock().unwrap();

        let old_path = Dir::get_writable_path(&mut state, old_name)?;

        let (old_name, dirent) = state.children.remove_entry(old_name)
            .expect("get_writable_path call above ensured the child exists");
        let result = new_dir.rename_and_move_target(&dirent, &old_path, new_name, cache);
        if result.is_err() {
            // "Roll back" any changes we did to the current directory because the rename could not
            // be completed on the target.
            state.children.insert(old_name, dirent);
        }
        result
    }

    fn rename_and_move_target(&self, dirent: &Dirent, old_path: &Path, new_name: &OsStr,
        cache: &dyn Cache) -> NodeResult<()> {
        // We are locking the target node while the source node is already locked, so this can
        // deadlock.  The previous Go implementation of this code ordered the locks based on inode
        // numbers so we should do the same thing here, but then this two-phase move implementation
        // does not work.
        //
        // The benefit of this implementation, however, is that the rename-and-move operation is
        // oblivious to the type of the target node type.  We just delegate part of the move to the
        // target, which can choose to operate as necessary.
        //
        // TODO(jmmv): Redo this once FUSE operations can be called concurrently, which at the
        // moment are serialized because the FUSE library we use does not support concurrency.  We
        // have an integration test to catch this race, which will ensure this doesn't go unnoticed.
        let mut state = self.state.lock().unwrap();

        let new_path = Dir::get_writable_path(&mut state, new_name)?;

        fs::rename(&old_path, &new_path)?;

        dirent.node.set_underlying_path(&new_path, cache);
        state.children.insert(new_name.to_owned(), dirent.clone());
        Ok(())
    }

    fn rmdir(&self, name: &OsStr, cache: &dyn Cache) -> NodeResult<()> {
        // TODO(jmmv): Figure out how to remove the redundant closure.
        #[allow(clippy::redundant_closure)]
        self.remove_any(name, |p| fs::remove_dir(p), cache)
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
            None => Err(KernelError::from_errno(errno::Errno::EACCES)),
        }
    }

    fn symlink(&self, name: &OsStr, link: &Path, uid: unistd::Uid, gid: unistd::Gid,
        ids: &IdGenerator, cache: &dyn Cache) -> NodeResult<(ArcNode, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        create_as(&path, uid, gid, |p| unix_fs::symlink(link, &p), |p| fs::remove_file(&p))?;
        Dir::post_create_lookup(self.writable, &mut state, &path, name,
            fuse::FileType::Symlink, ids, cache)
    }

    fn unlink(&self, name: &OsStr, cache: &dyn Cache) -> NodeResult<()> {
        // TODO(jmmv): Figure out how to remove the redundant closure.
        #[allow(clippy::redundant_closure)]
        self.remove_any(name, |p| fs::remove_file(p), cache)
    }
}
