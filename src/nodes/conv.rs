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

use fuse;
use libc;
use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::Path;
use std::time::{SystemTime, UNIX_EPOCH};
use time::Timespec;

/// Fixed point in time to use when we fail to interpret file system supplied timestamps.
const BAD_TIME: Timespec = Timespec { sec: 0, nsec: 0 };

/// Converts a system time as represented in a fs::Metadata object to a Timespec.
///
/// `path` is the file from which the timestamp was originally extracted and `name` represents the
/// metadata field the timestamp corresponds to; both are used for debugging purposes only.
///
/// If the given system time is missing, or if it is invalid, logs a warning and returns a fixed
/// time.  It is reasonable for these details to be missing because the backing file systems do
/// not always implement all possible file timestamps.
fn system_time_to_timespec(path: &Path, name: &str, time: &io::Result<SystemTime>) -> Timespec {
    match time {
        Ok(time) => match time.duration_since(UNIX_EPOCH) {
            Ok(duration) => Timespec::new(duration.as_secs() as i64,
                                          duration.subsec_nanos() as i32),
            Err(e) => {
                warn!("File system returned {} {:?} for {:?} that's before the Unix epoch: {}",
                      name, time, path, e);
                BAD_TIME
            }
        },
        Err(e) => {
            warn!("File system did not return a {} timestamp for {:?}: {}", name, path, e);
            BAD_TIME
        },
    }
}

/// Converts a file type as returned by the file system to a FUSE file type.
///
/// `path` is the file from which the file type was originally extracted and is only for debugging
/// purposes.
///
/// If the given file type cannot be mapped to a FUSE file type (because we don't know about that
/// type or, most likely, because the file type is bogus), logs a warning and returns a regular
/// file type with the assumption that most operations should work on it.
fn filetype_fs_to_fuse(path: &Path, fs_type: &fs::FileType) -> fuse::FileType {
    if fs_type.is_block_device() {
        fuse::FileType::BlockDevice
    } else if fs_type.is_char_device() {
        fuse::FileType::CharDevice
    } else if fs_type.is_dir() {
        fuse::FileType::Directory
    } else if fs_type.is_fifo() {
        fuse::FileType::NamedPipe
    } else if fs_type.is_file() {
        fuse::FileType::RegularFile
    } else if fs_type.is_socket() {
        fuse::FileType::Socket
    } else if fs_type.is_symlink() {
        fuse::FileType::Symlink
    } else {
        warn!("File system returned invalid file type {:?} for {:?}", fs_type, path);
        fuse::FileType::RegularFile
    }
}

/// Converts metadata attributes supplied by the file system to a FUSE file attributes tuple.
///
/// `inode` is the value of the FUSE inode (not the value of the inode supplied within `attr`) to
/// fill into the returned file attributes.  `path` is the file from which the attributes were
/// originally extracted and is only for debugging purposes.
///
/// Any errors encountered along the conversion process are logged and the corresponding field is
/// replaced by a reasonable value that should work.  In other words: all errors are swallowed.
pub fn attr_fs_to_fuse(path: &Path, inode: u64, attr: &fs::Metadata) -> fuse::FileAttr {
    let nlink = if attr.is_dir() {
        2  // "." entry plus whichever initial named node points at this.
    } else {
        1  // We don't support hard links so this is always valid.
    };

    let len = if attr.is_dir() {
        2  // TODO(jmmv): Reevaluate what directory sizes should be.
    } else {
        attr.len()
    };

    // TODO(jmmv): Using the underlying ctimes is slightly wrong because the ctimes track changes
    // to the inodes.  In most cases, operations that flow via sandboxfs will affect the underlying
    // ctime and propagate through here, which is fine, but other operations are purely in-memory.
    // To properly handle those cases, we should have our own ctime handling.
    let ctime = Timespec { sec: attr.ctime(), nsec: attr.ctime_nsec() as i32 };

    let perm = match attr.permissions().mode() {
        // TODO(https://github.com/rust-lang/rust/issues/51577): Drop :: prefix.
        mode if mode > ::std::u16::MAX as u32 => {
            warn!("File system returned mode {} for {:?}, which is too large; set to 0400",
                mode, path);
            0o400
        },
        mode => (mode as u16) & !libc::S_IFMT,
    };

    let rdev = match attr.rdev() {
        // TODO(https://github.com/rust-lang/rust/issues/51577): Drop :: prefix.
        rdev if rdev > ::std::u32::MAX as u64 => {
            warn!("File system returned rdev {} for {:?}, which is too large; set to 0",
                rdev, path);
            0
        },
        rdev => rdev as u32,
    };

    fuse::FileAttr {
        ino: inode,
        kind: filetype_fs_to_fuse(path, &attr.file_type()),
        nlink: nlink,
        size: len,
        blocks: 0, // TODO(jmmv): Reevaluate what blocks should be.
        atime: system_time_to_timespec(path, "atime", &attr.accessed()),
        mtime: system_time_to_timespec(path, "mtime", &attr.modified()),
        ctime: ctime,
        crtime: system_time_to_timespec(path, "crtime", &attr.created()),
        perm: perm,
        uid: attr.uid(),
        gid: attr.gid(),
        rdev: rdev,
        flags: 0,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::ffi::CString;
    use std::fs::File;
    use std::io::Write;
    use std::os::unix;
    use std::time::Duration;
    use tempdir::TempDir;

    #[test]
    fn test_system_time_to_timespec_ok() {
        let sys_time = SystemTime::UNIX_EPOCH + Duration::new(12345, 6789);
        let timespec = system_time_to_timespec(
            &Path::new("irrelevant"), "irrelevant", &Ok(sys_time));
        assert_eq!(Timespec { sec: 12345, nsec: 6789 }, timespec);
    }

    #[test]
    fn test_system_time_to_timespec_bad() {
        let sys_time = SystemTime::UNIX_EPOCH - Duration::new(1, 0);
        let timespec = system_time_to_timespec(
            &Path::new("irrelevant"), "irrelevant", &Ok(sys_time));
        assert_eq!(BAD_TIME, timespec);
    }

    #[test]
    fn test_system_time_to_timespec_missing() {
        let timespec = system_time_to_timespec(
            &Path::new("irrelevant"), "irrelevant",
            &Err(io::Error::from_raw_os_error(libc::ENOENT)));
        assert_eq!(BAD_TIME, timespec);
    }

    /// Sets the permissions of a file to exactly those given.
    fn chmod(path: &Path, perm: libc::mode_t) {
        let path = path.as_os_str().to_str().unwrap().as_bytes();
        let path = CString::new(path).unwrap();
        assert_eq!(0, unsafe { libc::chmod(path.as_ptr(), perm) });
    }

    /// Creates a block or character device and enforces that it is created successfully.
    fn mkdev(path: &Path, type_mask: libc::mode_t, dev: libc::dev_t) {
        assert!(type_mask == libc::S_IFBLK || type_mask == libc::S_IFCHR);
        let path = path.as_os_str().to_str().unwrap().as_bytes();
        let path = CString::new(path).unwrap();
        assert_eq!(0, unsafe { libc::mknod(path.as_ptr(), 0o444 | type_mask, dev) });
    }

    /// Creates a named pipe and enforces that it is created successfully.
    fn mkfifo(path: &Path) {
        let path = path.as_os_str().to_str().unwrap().as_bytes();
        let path = CString::new(path).unwrap();
        assert_eq!(0, unsafe { libc::mkfifo(path.as_ptr(), 0o444) });
    }

    /// Sets the atime and mtime of a file to the given values.
    fn utimes(path: &Path, atime: &Timespec, mtime: &Timespec) {
        let times = [
            libc::timeval { tv_sec: atime.sec, tv_usec: atime.nsec / 1000 },
            libc::timeval { tv_sec: mtime.sec, tv_usec: mtime.nsec / 1000 },
        ];
        let path = path.as_os_str().to_str().unwrap().as_bytes();
        let path = CString::new(path).unwrap();
        assert_eq!(0, unsafe { libc::utimes(path.as_ptr(), &times[0]) });
    }

    /// Runs a test for `filetype_fs_to_fuse_test` for a single file type.
    ///
    /// `exp_type` is the expected return value of the function call, and `create_entry` is a
    /// lambda that should create a file of the desired type in the path given to it.
    fn do_filetype_fs_to_fuse_test<T: Fn(&Path) -> ()>(exp_type: fuse::FileType, create_entry: T) {
        let dir = TempDir::new("test").unwrap();
        let path = dir.path().join("entry");
        create_entry(&path);
        let fs_type = fs::symlink_metadata(&path).unwrap().file_type();
        assert_eq!(exp_type, filetype_fs_to_fuse(&path, &fs_type));
    }

    #[test]
    fn test_filetype_fs_to_fuse_blockdevice() {
        let uid = unsafe { libc::getuid() } as u32;
        if uid != 0 {
            warn!("Not running as root; cannot create a block device");
            return;
        }

        do_filetype_fs_to_fuse_test(
            fuse::FileType::BlockDevice, |path| { mkdev(&path, libc::S_IFBLK, 50); });
    }

    #[test]
    fn test_filetype_fs_to_fuse_chardevice() {
        let uid = unsafe { libc::getuid() } as u32;
        if uid != 0 {
            warn!("Not running as root; cannot create a char device");
            return;
        }

        do_filetype_fs_to_fuse_test(
            fuse::FileType::CharDevice, |path| { mkdev(&path, libc::S_IFCHR, 50); });
    }

    #[test]
    fn test_filetype_fs_to_fuse_directory() {
        do_filetype_fs_to_fuse_test(
            fuse::FileType::Directory, |path| { fs::create_dir(&path).unwrap(); });
    }

    #[test]
    fn test_filetype_fs_to_fuse_namedpipe() {
        do_filetype_fs_to_fuse_test(
            fuse::FileType::NamedPipe, |path| { mkfifo(&path); });
    }

    #[test]
    fn test_filetype_fs_to_fuse_regular() {
        do_filetype_fs_to_fuse_test(
            fuse::FileType::RegularFile, |path| { File::create(path).unwrap(); });
    }

    #[test]
    fn test_filetype_fs_to_fuse_socket() {
        do_filetype_fs_to_fuse_test(
            fuse::FileType::Socket, |path| { unix::net::UnixListener::bind(&path).unwrap(); });
    }

    #[test]
    fn test_filetype_fs_to_fuse_symlink() {
        do_filetype_fs_to_fuse_test(
            fuse::FileType::Symlink, |path| { unix::fs::symlink("irrelevant", &path).unwrap(); });
    }

    /// Asserts that two FUSE file attributes are equal.
    ///
    /// TODO(jmmv): Upstream an Eq implementation for fuse::Fileattr so that this becomes more
    /// reliable and can provide nicer diagnostics on differences.
    fn assert_fileattrs_eq(attr1: &fuse::FileAttr, attr2: &fuse::FileAttr) {
        assert_eq!(attr1.ino, attr2.ino);
        assert_eq!(attr1.kind, attr2.kind);
        assert_eq!(attr1.nlink, attr2.nlink);
        assert_eq!(attr1.size, attr2.size);
        assert_eq!(attr1.blocks, attr2.blocks);
        assert_eq!(attr1.atime, attr2.atime);
        assert_eq!(attr1.mtime, attr2.mtime);
        assert_eq!(attr1.ctime, attr2.ctime);
        assert_eq!(attr1.crtime, attr2.crtime);
        assert_eq!(attr1.perm, attr2.perm);
        assert_eq!(attr1.uid, attr2.uid);
        assert_eq!(attr1.gid, attr2.gid);
        assert_eq!(attr1.rdev, attr2.rdev);
        assert_eq!(attr1.flags, attr2.flags);
    }

    #[test]
    fn test_attr_fs_to_fuse_directory() {
        let dir = TempDir::new("test").unwrap();
        let path = dir.path().join("root");
        fs::create_dir(&path).unwrap();
        fs::create_dir(path.join("subdir1")).unwrap();
        fs::create_dir(path.join("subdir2")).unwrap();

        chmod(&path, 0o750);
        let atime = Timespec { sec: 12345, nsec: 0 };
        let mtime = Timespec { sec: 678, nsec: 0 };
        utimes(&path, &atime, &mtime);

        let exp_attr = fuse::FileAttr {
            ino: 1234,  // Ensure underlying inode is not propagated.
            kind: fuse::FileType::Directory,
            nlink: 2, // TODO(jmmv): Should account for subdirs.
            size: 2,
            blocks: 0,
            atime: atime,
            mtime: mtime,
            ctime: BAD_TIME,
            crtime: BAD_TIME,
            perm: 0o750,
            uid: unsafe { libc::getuid() },
            gid: unsafe { libc::getgid() },
            rdev: 0,
            flags: 0,
        };

        let mut attr = attr_fs_to_fuse(&path, 1234, &fs::symlink_metadata(&path).unwrap());
        // We cannot really make any useful assertions on ctime and crtime as these cannot be
        // modified and may not be queryable, so stub them out.
        attr.ctime = BAD_TIME;
        attr.crtime = BAD_TIME;
        assert_fileattrs_eq(&exp_attr, &attr);
    }

    #[test]
    fn test_attr_fs_to_fuse_regular() {
        let dir = TempDir::new("test").unwrap();
        let path = dir.path().join("file");

        let mut file = File::create(&path).unwrap();
        let content = "Some text\n";
        file.write_all(content.as_bytes()).unwrap();
        drop(file);

        chmod(&path, 0o640);
        let atime = Timespec { sec: 54321, nsec: 0 };
        let mtime = Timespec { sec: 876, nsec: 0 };
        utimes(&path, &atime, &mtime);

        let exp_attr = fuse::FileAttr {
            ino: 42,  // Ensure underlying inode is not propagated.
            kind: fuse::FileType::RegularFile,
            nlink: 1,
            size: content.len() as u64,
            blocks: 0,
            atime: atime,
            mtime: mtime,
            ctime: BAD_TIME,
            crtime: BAD_TIME,
            perm: 0o640,
            uid: unsafe { libc::getuid() },
            gid: unsafe { libc::getgid() },
            rdev: 0,
            flags: 0,
        };

        let mut attr = attr_fs_to_fuse(&path, 42, &fs::symlink_metadata(&path).unwrap());
        // We cannot really make any useful assertions on ctime and crtime as these cannot be
        // modified and may not be queryable, so stub them out.
        attr.ctime = BAD_TIME;
        attr.crtime = BAD_TIME;
        assert_fileattrs_eq(&exp_attr, &attr);
    }
}
