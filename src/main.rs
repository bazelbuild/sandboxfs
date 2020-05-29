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

//! Command-line interface for the sandboxfs file system.

// Keep these in sync with the list of checks in lib.rs.
#![warn(bad_style, missing_docs)]
#![warn(unused, unused_extern_crates, unused_import_braces, unused_qualifications)]
#![warn(unsafe_code)]

extern crate env_logger;
#[macro_use] extern crate failure;
extern crate getopts;
#[macro_use] extern crate log;
extern crate sandboxfs;
extern crate time;

use failure::{Fallible, ResultExt};
use getopts::Options;
use std::env;
use std::path::{Path, PathBuf};
use std::process;
use std::result::Result;
use std::sync::Arc;
use time::Timespec;

/// Default value of the `--input` and `--output` flags.
static DEFAULT_INOUT: &str = "-";

/// Default value of the `--ttl` flag.
///
/// This is expressed as a string rather than a parsed value to ensure the default value can be
/// parsed with the same semantics as user-provided values.
static DEFAULT_TTL: &str = "60s";

/// Suffix for durations expressed in seconds.
static SECONDS_SUFFIX: &str = "s";

/// Execution failure due to a user-triggered error.
#[derive(Debug, Fail)]
#[fail(display = "{}", message)]
struct UsageError {
    message: String,
}

/// Parses the value of a flag that controls who has access to the mount point.
///
/// Returns the collection of options, if any, to be passed to the FUSE mount operation in order to
/// grant the requested permissions.
fn parse_allow(s: &str) -> Fallible<&'static [&'static str]> {
    match s {
        "other" => Ok(&["-o", "allow_other"]),
        "root" => {
            if cfg!(target_os = "linux") {
                // "-o allow_root" is broken on Linux because this is not actually a
                // fusermount option: it is a libfuse option and the Go bindings don't
                // implement it as such.  We could implement this on our own by handling
                // allow_root as if it were allow_other with an explicit user check... but
                // it's probably not worth doing.  For now, just tell the user that we know
                // about the breakage.
                //
                // See https://github.com/bazil/fuse/issues/144 for context (which is about Go
                // but applies equally here).
                Err(format_err!("--allow=root is known to be broken on Linux"))
            } else {
                Ok(&["-o", "allow_root"])
            }
        },
        "self" => Ok(&[]),
        _ => {
            let message = format!("{} must be one of other, root, or self", s);
            Err(UsageError { message }.into())
        },
    }
}

/// Parses the value of a flag that takes a duration, which must specify its unit.
fn parse_duration(s: &str) -> Result<Timespec, UsageError> {
    let (value, unit) = match s.find(|c| !char::is_ascii_digit(&c) && c != '-') {
        Some(pos) => s.split_at(pos),
        None => {
            let message = format!("invalid time specification {}: missing unit", s);
            return Err(UsageError { message });
        },
    };

    if unit != SECONDS_SUFFIX {
        let message = format!(
            "invalid time specification {}: unsupported unit '{}' (only '{}' is allowed)",
            s, unit, SECONDS_SUFFIX);
        return Err(UsageError { message });
    }

    value.parse::<u32>()
        .map(|sec| Timespec { sec: i64::from(sec), nsec: 0 })
        .map_err(|e| UsageError { message: format!("invalid time specification {}: {}", s, e) })
}

/// Takes the list of strings that represent mappings (supplied via multiple instances of the
/// `--mapping` flag) and returns a parsed representation of those flags.
fn parse_mappings<T: AsRef<str>, U: IntoIterator<Item=T>>(args: U)
    -> Result<Vec<sandboxfs::Mapping>, UsageError> {
    let mut mappings = Vec::new();

    for arg in args {
        let arg = arg.as_ref();

        let fields: Vec<&str> = arg.split(':').collect();
        if fields.len() != 3 {
            let message = format!("bad mapping {}: expected three colon-separated fields", arg);
            return Err(UsageError { message });
        }

        let writable = {
            if fields[0] == "ro" {
                false
            } else if fields[0] == "rw" {
                true
            } else {
                let message = format!("bad mapping {}: type was {} but should be ro or rw",
                    arg, fields[0]);
                return Err(UsageError { message });
            }
        };

        let path = PathBuf::from(fields[1]);
        let underlying_path = PathBuf::from(fields[2]);

        match sandboxfs::Mapping::from_parts(path, underlying_path, writable) {
            Ok(mapping) => mappings.push(mapping),
            Err(e) => {
                // TODO(jmmv): Figure how to best leverage failure's cause propagation.  May need
                // to define a custom ErrorKind to represent UsageError, instead of having a special
                // error type.
                let message = format!("bad mapping {}: {}", arg, e);
                return Err(UsageError { message });
            }
        }
    }

    Ok(mappings)
}

/// Obtains the program name from the execution's first argument, or returns a default if the
/// program name cannot be determined for whatever reason.
fn program_name(args: &[String], default: &'static str) -> String {
    let default = String::from(default);
    match args.get(0) {
        Some(arg0) => match Path::new(arg0).file_name() {
            Some(basename) => match basename.to_str() {
                Some(basename) => String::from(basename),
                None => default,
            },
            None => default,
        },
        None => default,
    }
}

/// Parses the value of a flag specifying a file for I/O.
///
/// `value` contains the textual value of the flag, which is returned as a path if present.
/// Otherwise, if `value` is missing or if it matches `DEFAULT_INOUT`, then returns None.
///
/// Note that, for simplicity, this will reopen any of the standard streams if provided as a path
/// to `/dev/std*`.  Even though that's discouraged (because reopening these devices under `sudo` is
/// not possible), we cannot reliably prevent the user from supplying those paths.
fn file_flag(value: &Option<String>) -> Option<PathBuf> {
    value.as_ref().and_then(
        |path| if path == DEFAULT_INOUT { None } else { Some(PathBuf::from(path) )})
}

/// Prints program usage information to stdout.
fn usage(program: &str, opts: &Options) {
    let brief = format!("Usage: {} [options] MOUNT_POINT", program);
    print!("{}", opts.usage(&brief));
}

/// Prints version information to stdout.
fn version() {
    println!("{} {}", env!("CARGO_PKG_NAME"), env!("CARGO_PKG_VERSION"));
}

/// Program's entry point.  This is a "safe" version of `main` in the sense that this doesn't
/// directly handle errors: all errors are returned to the caller for consistent reporter to the
/// user depending on their type.
fn safe_main(program: &str, args: &[String]) -> Fallible<()> {
    env_logger::init();

    let cpus = num_cpus::get();

    let mut opts = Options::new();
    opts.optopt("", "allow", concat!("specifies who should have access to the file system",
        " (default: self)"), "other|root|self");
    opts.optopt("", "cpu_profile", "enables CPU profiling and writes a profile to the given path",
        "PATH");
    opts.optflag("", "help", "prints usage information and exits");
    opts.optopt("", "input",
        &format!("where to read reconfiguration data from ({} for stdin)", DEFAULT_INOUT),
        "PATH");
    opts.optmulti("", "mapping", "type and locations of a mapping", "TYPE:PATH:UNDERLYING_PATH");
    opts.optflag("", "node_cache", "enables the path-based node cache (known broken)");
    opts.optopt("", "output",
        &format!("where to write the reconfiguration status to ({} for stdout)", DEFAULT_INOUT),
        "PATH");
    opts.optopt("", "reconfig_threads",
        &format!("number of reconfiguration threads (default: {})", cpus), "COUNT");
    opts.optopt("", "ttl",
        &format!("how long the kernel is allowed to keep file metadata (default: {})", DEFAULT_TTL),
        &format!("TIME{}", SECONDS_SUFFIX));
    opts.optflag("", "version", "prints version information and exits");
    opts.optflag("", "xattrs", "enables support for extended attributes");
    let matches = opts.parse(args)?;

    if matches.opt_present("help") {
        usage(&program, &opts);
        return Ok(());
    }

    if matches.opt_present("version") {
        version();
        return Ok(());
    }

    let mut options = vec!("-o", "fsname=sandboxfs");
    // TODO(jmmv): Support passing in arbitrary FUSE options from the command line, like "-o ro".

    if let Some(value) = matches.opt_str("allow") {
        for arg in parse_allow(&value)? {
            options.push(arg);
        }
    }

    let mappings = parse_mappings(matches.opt_strs("mapping"))?;

    let ttl = match matches.opt_str("ttl") {
        Some(value) => parse_duration(&value)?,
        None => parse_duration(DEFAULT_TTL).expect(
            "default value for flag is not accepted by the parser; this is a bug in the value"),
    };

    let input = {
        let input_flag = matches.opt_str("input");
        sandboxfs::open_input(file_flag(&input_flag))
            .with_context(|_| format!("Failed to open reconfiguration input '{}'",
                input_flag.unwrap_or_else(|| DEFAULT_INOUT.to_owned())))?
    };

    let output = {
        let output_flag = matches.opt_str("output");
        sandboxfs::open_output(file_flag(&output_flag))
            .with_context(|_| format!("Failed to open reconfiguration output '{}'",
                output_flag.unwrap_or_else(|| DEFAULT_INOUT.to_owned())))?
    };

    let reconfig_threads = match matches.opt_str("reconfig_threads") {
        Some(value) => {
            match value.parse::<usize>() {
                Ok(n) => n,
                Err(e) => return Err(UsageError {
                    message: format!("invalid thread count {}: {}", value, e)
                }.into()),
            }
        },
        None => cpus,
    };

    let mount_point = if matches.free.len() == 1 {
        Path::new(&matches.free[0])
    } else {
        return Err(UsageError { message: "invalid number of arguments".to_string() }.into());
    };

    let node_cache: sandboxfs::ArcCache = if matches.opt_present("node_cache") {
        warn!("Using --node_cache is known to be broken under certain scenarios; see the manpage");
        Arc::from(sandboxfs::PathCache::default())
    } else {
        Arc::from(sandboxfs::NoCache::default())
    };

    let _profiler;
    if let Some(path) = matches.opt_str("cpu_profile") {
        _profiler = sandboxfs::ScopedProfiler::start(&path).context("Failed to start CPU profile")?;
    };
    sandboxfs::mount(
        mount_point, &options, &mappings, ttl, node_cache, matches.opt_present("xattrs"),
        input, output, reconfig_threads)
        .with_context(|_| format!("Failed to mount {}", mount_point.display()))?;
    Ok(())
}

/// Program's entry point.  This delegates to `safe_main` for all program logic and is just in
/// charge of consistently formatting and reporting all possible errors to the caller.
fn main() {
    let args: Vec<String> = env::args().collect();
    let program = program_name(&args, "sandboxfs");

    if let Err(err) = safe_main(&program, &args[1..]) {
        if let Some(err) = err.downcast_ref::<UsageError>() {
            eprintln!("Usage error: {}", err);
            eprintln!("Type {} --help for more information", program);
            process::exit(2);
        } else if let Some(err) = err.downcast_ref::<getopts::Fail>() {
            eprintln!("Usage error: {}", err);
            eprintln!("Type {} --help for more information", program);
            process::exit(2);
        } else {
            eprintln!("{}: {}", program, sandboxfs::flatten_causes(&err));
            process::exit(1);
        }
    }
}

#[cfg(test)]
mod tests {
    use sandboxfs::Mapping;
    use super::*;

    /// Checks that an error, once formatted for printing, contains the given substring.
    fn err_contains(substr: &str, err: impl failure::Fail) {
        let formatted = format!("{}", err);
        assert!(formatted.contains(substr),
            "bad error message '{}'; does not contain '{}'", formatted, substr);
    }

    #[test]
    fn test_parse_duration_ok() {
        assert_eq!(Timespec { sec: 1234, nsec: 0 }, parse_duration("1234s").unwrap());
    }

    #[test]
    fn test_parse_duration_bad_unit() {
        err_contains("missing unit", parse_duration("1234").unwrap_err());
        err_contains("unsupported unit 'ms'", parse_duration("1234ms").unwrap_err());
        err_contains("unsupported unit 'ss'", parse_duration("1234ss").unwrap_err());
    }

    #[test]
    fn test_parse_duration_bad_value() {
        err_contains("invalid digit", parse_duration("-5s").unwrap_err());
        err_contains("unsupported unit ' s'", parse_duration("5 s").unwrap_err());
        err_contains("unsupported unit ' 5s'", parse_duration(" 5s").unwrap_err());
    }

    #[test]
    fn test_parse_mappings_ok() {
        let args = ["ro:/:/fake/root", "rw:/foo:/bar"];
        let exp_mappings = vec!(
            Mapping::from_parts(PathBuf::from("/"), PathBuf::from("/fake/root"), false).unwrap(),
            Mapping::from_parts(PathBuf::from("/foo"), PathBuf::from("/bar"), true).unwrap(),
        );
        match parse_mappings(&args) {
            Ok(mappings) => assert_eq!(exp_mappings, mappings),
            Err(e) => panic!(e),
        }
    }

    #[test]
    fn test_parse_mappings_bad_format() {
        for arg in ["", "foo:bar", "foo:bar:baz:extra"].iter() {
            let err = parse_mappings(&[arg]).unwrap_err();
            err_contains(
                &format!("bad mapping {}: expected three colon-separated fields", arg), err);
        }
    }

    #[test]
    fn test_parse_mappings_bad_type() {
        let args = ["rr:/foo:/bar"];
        let err = parse_mappings(&args).unwrap_err();
        err_contains("bad mapping rr:/foo:/bar: type was rr but should be ro or rw", err);
    }

    #[test]
    fn test_parse_mappings_bad_path() {
        let args = ["ro:foo:/bar"];
        let err = parse_mappings(&args).unwrap_err();
        err_contains("bad mapping ro:foo:/bar: path \"foo\" is not absolute", err);
    }

    #[test]
    fn test_parse_mappings_bad_underlying_path() {
        let args = ["ro:/foo:bar"];
        let err = parse_mappings(&args).unwrap_err();
        err_contains("bad mapping ro:/foo:bar: path \"bar\" is not absolute", err);
    }

    #[test]
    fn test_program_name_uses_default_on_errors() {
        assert_eq!("default", program_name(&[], "default"));
    }

    #[test]
    fn test_program_name_uses_file_name_only() {
        assert_eq!("b", program_name(&["a/b".to_string()], "unused"));
        assert_eq!("foo", program_name(&["./x/y/foo".to_string()], "unused"));
    }
}
