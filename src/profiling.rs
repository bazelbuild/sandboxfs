// Copyright 2019 Google Inc.
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

#[cfg(feature = "profiling")] use cpuprofiler::PROFILER;
use failure::Fallible;
use std::path::Path;

/// Facade for `cpuprofiler::PROFILER` to cope with the optional gperftools dependency and to
/// ensure profiling stops on `drop`.
pub struct ScopedProfiler {}

impl ScopedProfiler {
    #[cfg(not(feature = "profiling"))]
    fn real_start<P: AsRef<Path>>(_path: P) -> Fallible<ScopedProfiler> {
        Err(format_err!("Compile-time \"profiling\" feature not enabled"))
    }

    #[cfg(feature = "profiling")]
    fn real_start<P: AsRef<Path>>(path: P) -> Fallible<ScopedProfiler> {
        let path = path.as_ref();
        let path_str = match path.to_str() {
            Some(path_str) => path_str,
            None => return Err(format_err!("Invalid path {}", path.display())),
        };
        let mut profiler = PROFILER.lock().unwrap();
        info!("Starting CPU profiler and writing results to {}", path_str);
        profiler.start(path_str.as_bytes()).unwrap();
        Ok(ScopedProfiler {})
    }

    /// Starts the CPU profiler and stores the profile in the given `path`.
    ///
    /// This will fail if sandboxfs was built without the "profiler" feature.  This may fail if
    /// there are problems initializing the profiler.
    ///
    /// Note that, due to the nature of profiling, there can only be one `ScopedPointer` active at
    /// any given time.  Trying to create two instances of this will cause this method to block
    /// until the other object is dropped.
    pub fn start<P: AsRef<Path>>(path: P) -> Fallible<ScopedProfiler> {
        ScopedProfiler::real_start(path)
    }

    #[cfg(not(feature = "profiling"))]
    fn real_stop(&mut self) {
    }

    #[cfg(feature = "profiling")]
    fn real_stop(&mut self) {
        let mut profiler = PROFILER.lock().unwrap();
        profiler.stop().expect("Profiler apparently not active, but it must have been");
        info!("CPU profiler stopped");
    }
}

impl Drop for ScopedProfiler {
    fn drop(&mut self) {
        self.real_stop()
    }
}
