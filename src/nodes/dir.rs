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

use {Cache, IdGenerator};
use failure::{Error, ResultExt};
use nix::{errno, fcntl, sys, unistd};
use nix::dir as rawdir;
use nodes::{AttrDelta, Handle, KernelError, Node, NodeResult, conv, setattr};
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

/// Handle for an open directory.
struct OpenDir {
    // These are copies of the fields that also exist in the Dir corresponding to this OpenDir.
    // Ideally we could just hold an immutable reference to the Dir instance... but this is hard
    // to do because, when opendir() gets called on an abstract Node, we do not get access to the
    // Arc<Node> that corresponds to it (to clone it).  Given that these values are immutable on
    // the node, holding a copy here is fine.
    inode: u64,
    writable: bool,
    state: Arc<Mutex<MutableDir>>,

    /// Handle for the open directory file descriptor.  This is `None` if the directory does not
    /// have an underlying path.
    handle: Mutex<Option<rawdir::Dir>>,
}

impl Handle for OpenDir {
    fn readdir(&self, ids: &IdGenerator, cache: &Cache, reply: &mut fuse::ReplyDirectory)
        -> NodeResult<()> {
        let mut state = self.state.lock().unwrap();

        reply.add(self.inode, 0, fuse::FileType::Directory, ".");
        reply.add(state.parent, 1, fuse::FileType::Directory, "..");
        let mut pos = 2;

        // First, return the entries that correspond to explicit mappings performed by the user at
        // either mount time or during a reconfiguration.  Those should clobber any on-disk
        // contents that we discover later when we issue the readdir on the underlying directory,
        // if any.
        for (name, dirent) in &state.children {
            if dirent.explicit_mapping {
                reply.add(dirent.node.inode(), pos, dirent.node.file_type_cached(), name);
                pos += 1;
            }
        }

        let mut handle = self.handle.lock().unwrap();

        if handle.is_none() {
            debug_assert!(state.underlying_path.is_none());
            return Ok(());
        }
        debug_assert!(state.underlying_path.is_some());
        let handle = handle.as_mut().unwrap();
        for entry in handle.iter() {
            let entry = entry?;
            let name = entry.file_name();

            let name = OsStr::from_bytes(name.to_bytes()).to_os_string();

            if let Some(dirent) = state.children.get(&name) {
                if dirent.explicit_mapping {
                    // Found an on-disk entry that also corresponds to an explicit mapping by the
                    // user.  Nothing to do: we already handled this case above.
                    continue;
                }
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

            reply.add(child.inode(), pos, fs_type, &name);
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

            pos += 1;
        }
        // No need to worry about rewinding handle.iter() for future reads on the same OpenDir:
        // the rawdir::Dir implementation does this for us.
        Ok(())
    }
}

/// Representation of a directory entry.
struct Dirent {
    node: Arc<Node>,
    explicit_mapping: bool,
}

/// Representation of a directory node.
pub struct Dir {
    inode: u64,
    writable: bool,
    state: Arc<Mutex<MutableDir>>,
}

/// Holds the mutable data of a directory node.
struct MutableDir {
    parent: u64,
    underlying_path: Option<PathBuf>,
    attr: fuse::FileAttr,
    children: HashMap<OsString, Dirent>,
}

impl Dir {
    /// Creates a new scaffold directory to represent an in-memory directory.
    ///
    /// The directory's timestamps are set to `now` and the ownership is set to the current user.
    pub fn new_empty(inode: u64, parent: Option<&Node>, now: time::Timespec) -> Arc<Node> {
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
            parent: parent.map_or(inode, |node| node.inode()),
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
        -> Arc<Node> {
        if !fs_attr.is_dir() {
            panic!("Can only construct based on dirs");
        }
        let attr = conv::attr_fs_to_fuse(underlying_path, inode, &fs_attr);

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
        now: time::Timespec) -> Arc<Node> {
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
                warn!("Path {:?} backing a directory node is no longer a directory; got {:?}",
                    path, fs_attr.file_type());
                return Err(KernelError::from_errno(errno::Errno::EIO));
            }
            state.attr = conv::attr_fs_to_fuse(path, inode, &fs_attr);
        };

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
        cache: &Cache) -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
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
            let attr = conv::attr_fs_to_fuse(path.as_path(), node.inode(), &fs_attr);
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
        exp_type: fuse::FileType, ids: &IdGenerator, cache: &Cache)
        -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
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

    fn map(&self, components: &[Component], underlying_path: &Path, writable: bool,
        ids: &IdGenerator, cache: &Cache) -> Result<(), Error> {
        let (name, remainder) = split_components(components);

        let mut state = self.state.lock().unwrap();

        if let Some(dirent) = state.children.get(name) {
            // TODO(jmmv): We should probably mark this dirent as an explicit mapping if it already
            // wasn't, but the Go variant of this code doesn't do this -- so investigate later.
            ensure!(dirent.node.file_type_cached() == fuse::FileType::Directory, "Already mapped");
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
            Ok(())
        } else {
            ensure!(child.file_type_cached() == fuse::FileType::Directory, "Already mapped");
            child.map(remainder, underlying_path, writable, ids, cache)
        }
    }

    #[cfg_attr(feature = "cargo-clippy", allow(type_complexity))]
    fn create(&self, name: &OsStr, mode: u32, flags: u32, ids: &IdGenerator, cache: &Cache)
        -> NodeResult<(Arc<Node>, Arc<Handle>, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        let mut options = conv::flags_to_openoptions(flags, self.writable)?;
        options.create(true);
        options.mode(mode);

        let file = options.open(&path)?;
        let (node, attr) = Dir::post_create_lookup(self.writable, &mut state, &path, name,
            fuse::FileType::RegularFile, ids, cache)?;
        Ok((node, Arc::from(file), attr))
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        Dir::getattr_locked(self.inode, &mut state)
    }

    fn lookup(&self, name: &OsStr, ids: &IdGenerator, cache: &Cache)
        -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        Dir::lookup_locked(self.writable, &mut state, name, ids, cache)
    }

    fn mkdir(&self, name: &OsStr, mode: u32, ids: &IdGenerator, cache: &Cache)
        -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        fs::DirBuilder::new().mode(mode).create(&path)?;
        Dir::post_create_lookup(self.writable, &mut state, &path, name,
            fuse::FileType::Directory, ids, cache)
    }

    fn mknod(&self, name: &OsStr, mode: u32, rdev: u32, ids: &IdGenerator, cache: &Cache)
        -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
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
            sys::stat::SFlag::S_IFCHR => fuse::FileType::CharDevice,
            sys::stat::SFlag::S_IFBLK => fuse::FileType::BlockDevice,
            sys::stat::SFlag::S_IFIFO => fuse::FileType::NamedPipe,
            _ => {
                warn!("mknod received request to create {} with type {:?}, which is not supported",
                    path.display(), sflag);
                return Err(KernelError::from_errno(errno::Errno::EIO));
            },
        };

        #[cfg_attr(feature = "cargo-clippy", allow(cast_lossless))]
        sys::stat::mknod(&path, sflag, perm, rdev as sys::stat::dev_t)?;
        Dir::post_create_lookup(self.writable, &mut state, &path, name, exp_filetype, ids, cache)
    }

    fn open(&self, flags: u32) -> NodeResult<Arc<Handle>> {
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
        }))
    }

    fn setattr(&self, delta: &AttrDelta) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();

        // setattr only gets called on writable nodes, which must have an underlying path.  This
        // assertion will go away when we have the ability to delete nodes as those may still
        // exist in memory but have no corresponding underlying path.
        debug_assert!(state.underlying_path.is_some());

        state.attr = setattr(state.underlying_path.as_ref(), &state.attr, delta)?;
        Ok(state.attr)
    }

    fn symlink(&self, name: &OsStr, link: &Path, ids: &IdGenerator, cache: &Cache)
        -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        let mut state = self.state.lock().unwrap();
        let path = Dir::get_writable_path(&mut state, name)?;

        unix_fs::symlink(link, &path)?;
        Dir::post_create_lookup(self.writable, &mut state, &path, name,
            fuse::FileType::Symlink, ids, cache)
    }
}
