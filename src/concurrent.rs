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

use nix::unistd;
use std::fs;
use std::io;
use std::os::unix::io as unix_io;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

/// A file with a single scoped owner but with multiple non-owner views.
///
/// A `ShareableFile` object owns the file passed to it at construction time and will close the
/// underlying file handle when the object is dropped.
///
/// Concurrent views into this same file, obtained via the `clone_unowned` method, do not own the
/// file handle.  Such views must accept the fact that the handle can be closed at any time, a
/// condition that is simply exposed as if the handle reached EOF.  Concurrent views can safely be
/// moved across threads.
pub struct ShareableFile {
    /// Underlying file descriptor shared across all views of this file.
    fd: unix_io::RawFd,

    /// Whether this instance of the file is responsible for closing the file descriptor.
    owned: bool,

    /// Whether the file descriptor has already been closed or not.
    ///
    /// In principle, we don't need this field: concurrent readers of this file will get their
    /// reads abruptly terminated with an error, and we should be able to rely on EBADFD to tell
    /// that this happened because of us closing the file descriptor.  Unfortunately... macOS
    /// Mojave doesn't cooperate and yields very strange error codes (like EISDIR) pretty frequently
    /// (but not deterministically).  May it be an APFS bug?
    closed: Arc<AtomicBool>,
}

impl ShareableFile {
    /// Constructs a new `ShareableFile` from an open file and takes ownership of it.
    pub fn from(file: fs::File) -> ShareableFile {
        use std::os::unix::io::IntoRawFd;
        ShareableFile {
            fd: file.into_raw_fd(),
            owned: true,
            closed: Arc::from(AtomicBool::new(false)),
        }
    }

    /// Returns an unowned view of the file.
    ///
    /// Users of this file must accept that the file can be closed at any time by the owner.
    pub fn clone_unowned(&mut self) -> ShareableFile {
        ShareableFile {
            fd: self.fd,
            owned: false,
            closed: self.closed.clone(),
        }
    }
}

impl Drop for ShareableFile {
    fn drop(&mut self) -> () {
        if self.owned {
            debug!("Closing ShareableFile with fd {}", self.fd);

            // We must mark the file as closed before attempting to close it because, when we do
            // the actual close, any threads blocked on a read will get unlocked immediately and
            // we won't have a chance to update the status.  It doesn't matter if the close succeeds
            // or not though, because we cannot do anything useful in the latter case anyway.
            self.closed.store(true, Ordering::SeqCst);

            if let Err(e) = unistd::close(self.fd) {
                warn!("Failed to close fd {}: {}", self.fd, e);
            }
        }
    }
}

impl io::Read for ShareableFile {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        match unistd::read(self.fd, buf) {
            Ok(n) => Ok(n),
            Err(nix::Error::Sys(errno)) => {
                if self.closed.load(Ordering::SeqCst) {
                    Ok(0)  // Simulate EOF due to close in another thread.
                } else {
                    Err(io::Error::from_raw_os_error(errno as i32))
                }
            },
            Err(e) => panic!(
                "Did not expect to get an error without an errno from a nix call: {:?}", e),
        }
    }
}

#[cfg(test)]
mod tests {
    use nix::sys;
    use std::io::{Read, Write};
    use std::path::Path;
    use std::thread;
    use super::*;
    use tempfile;

    #[test]
    fn test_shareable_file_clones_share_descriptor_and_only_one_owns() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("file");
        fs::File::create(&path).unwrap().write_all(b"ABCDEFG").unwrap();

        fn read_one_byte(input: &mut impl Read) -> u8 {
            let mut buffer = [0];
            input.read_exact(&mut buffer).unwrap();
            buffer[0]
        }

        let mut file = ShareableFile::from(fs::File::open(&path).unwrap());
        let mut reader1 = file.clone_unowned();
        let mut reader2 = file.clone_unowned();
        assert_eq!(b'A', read_one_byte(&mut reader1));
        drop(reader1);  // Make sure dropping a non-owner copy doesn't close the file handle.
        assert_eq!(b'B', read_one_byte(&mut file));
        assert_eq!(b'C', read_one_byte(&mut reader2));
        drop(file);  // Closes the file descriptor so the readers should now not be able to read.
        let mut buffer = [0];
        assert_eq!(0, reader2.read(&mut buffer).expect("Expected 0 byte count as EOF after close"));
    }

    #[test]
    fn test_shareable_file_close_unblocks_reads_without_error() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("pipe");
        unistd::mkfifo(&path, sys::stat::Mode::S_IRUSR | sys::stat::Mode::S_IWUSR).unwrap();

        // We need to write to the FIFO for the read-only open below to not block.
        let writer_handle = {
            let path = path.clone();
            thread::spawn(move || {
                let mut writer = fs::File::create(&path).unwrap();
                write!(writer, "some text longer than we'll ever read")
            })
        };

        let mut file = ShareableFile::from(fs::File::open(&path).unwrap());

        let reader_handle = {
            let mut file = file.clone_unowned();
            thread::spawn(move || {
                let mut buffer = [0];
                file.read(&mut buffer)
            })
        };

        writer_handle.join().unwrap().expect("Write didn't finish successfully");
        drop(file);  // This should unblock the reader thread and let the join complete.
        reader_handle.join().unwrap().expect("Read didn't return success on EOF-like condition");
    }
}
