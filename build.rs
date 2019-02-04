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

extern crate pkg_config;

/// Configures the crate to link against `lib_name`.
///
/// The library is searched via the pkg-config file provided in `pc_name`, which provides us
/// accurate information on how to find the library to link to.
///
/// However, for libraries that do not provide a pkg-config file, `fallback` can be set to true to
/// just rely on the linker's search path to find it.  This is not accurate but is better than just
/// failing to build.
#[allow(unused)]
fn find_library(pc_name: &str, lib_name: &str, fallback: bool) {
    match pkg_config::Config::new().atleast_version("2.0").probe(pc_name) {
        Ok(_) => (),
        Err(_) => if fallback { println!("cargo:rustc-link-lib={}", lib_name) },
    };
}

fn main () {
    // Look for the libraries required by our cpuprofiler dependency.  Such dependency should do
    // this on its own but it doesn't yet.  Given that we just need this during linking, we can
    // cheat and do it ourselves.
    //
    // Note that older versions of gperftools (the package providing libprofiler) did not ship a
    // pkg-config file, so we must fall back to using the linker's path.
    //
    // TODO(https://github.com/AtheMathmo/cpuprofiler/pull/10): Remove this in favor of upstream
    // doing the right thing when this PR is accepted a new cpuprofiler version is released.
    // TODO(https://github.com/dignifiedquire/rust-gperftools/pull/1): Remove this in favor of
    // upstream doing the right thing when this PR is accepted and switch to rust-gperftools instead
    // (which has the added benefit of providing heap profiling).
    #[cfg(feature = "profiling")] find_library("libprofiler", "profiler", true);
}
