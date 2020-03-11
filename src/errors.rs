// Copyright 2020 Google Inc.
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

use failure::Error;
use nix::errno::Errno;
use std::io;
use std::path::PathBuf;

/// Type that represents an error understood by the kernel.
#[derive(Debug, Fail)]
#[fail(display = "errno={}", errno)]
pub struct KernelError {
    errno: Errno,
}

impl KernelError {
    /// Constructs a new error given a raw errno code.
    pub fn from_errno(errno: Errno) -> KernelError {
        KernelError { errno }
    }

    /// Obtains the errno code contained in this error as an integer.
    pub fn errno_as_i32(&self) -> i32 {
        self.errno as i32
    }
}

impl From<io::Error> for KernelError {
    fn from(e: io::Error) -> Self {
        match e.raw_os_error() {
            Some(errno) => KernelError::from_errno(Errno::from_i32(errno)),
            None => {
                warn!("Got io::Error without an errno; propagating as EIO: {}", e);
                KernelError::from_errno(Errno::EIO)
            },
        }
    }
}

impl From<nix::Error> for KernelError {
    fn from(e: nix::Error) -> Self {
        match e {
            nix::Error::Sys(errno) => KernelError::from_errno(errno),
            _ => {
                warn!("Got nix::Error without an errno; propagating as EIO: {}", e);
                KernelError::from_errno(Errno::EIO)
            }
        }
    }
}

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

#[cfg(test)]
mod tests {
    use super::*;

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
}
