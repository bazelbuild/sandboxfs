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

extern crate env_logger;
#[macro_use] extern crate failure;
extern crate getopts;
extern crate sandboxfs;

use failure::Error;
use getopts::Options;
use std::env;
use std::path::{Path, PathBuf};
use std::process;
use std::result::Result;

/// Execution failure due to a user-triggered error.
#[derive(Debug, Fail)]
#[fail(display = "{}", message)]
struct UsageError {
    message: String,
}

/// Takes the list of strings that represent mappings (supplied via multiple instances of the
/// `--mapping` flag) and returns a parsed representation of those flags.
fn parse_mappings<T: AsRef<str>, U: IntoIterator<Item=T>>(args: U)
    -> Result<Vec<sandboxfs::Mapping>, UsageError> {
    let mut mappings = Vec::new();

    for arg in args {
        let arg = arg.as_ref();

        let fields: Vec<&str> = arg.split(":").collect();
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

        match sandboxfs::Mapping::new(path, underlying_path, writable) {
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

/// Prints program usage information to stdout.
fn usage(program: &str, opts: &Options) {
    let brief = format!("Usage: {} [options] MOUNT_POINT", program);
    print!("{}", opts.usage(&brief));
}

/// Program's entry point.  This is a "safe" version of `main` in the sense that this doesn't
/// directly handle errors: all errors are returned to the caller for consistent reporter to the
/// user depending on their type.
fn safe_main(program: &str, args: &[String]) -> Result<(), Error> {
    env_logger::init();

    let mut opts = Options::new();
    opts.optflag("", "help", "prints usage information and exits");
    opts.optmulti("", "mapping", "type and locations of a mapping", "TYPE:PATH:UNDERLYING_PATH");
    let matches = opts.parse(args)?;

    if matches.opt_present("help") {
        usage(&program, &opts);
        return Ok(());
    }

    let mappings = parse_mappings(matches.opt_strs("mapping"))?;

    let mount_point = if matches.free.len() == 1 {
        &matches.free[0]
    } else {
        return Err(Error::from(UsageError { message: "invalid number of arguments".to_string() }));
    };

    sandboxfs::mount(Path::new(mount_point), &mappings)?;
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
            eprintln!("{}: {}", program, err);
            process::exit(1);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use super::sandboxfs::Mapping;

    #[test]
    fn test_parse_mappings_ok() {
        let args = ["ro:/:/fake/root", "rw:/foo:/bar"];
        let exp_mappings = vec!(
            Mapping::new(PathBuf::from("/"), PathBuf::from("/fake/root"), false).unwrap(),
            Mapping::new(PathBuf::from("/foo"), PathBuf::from("/bar"), true).unwrap(),
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
            assert_eq!(
                format!("bad mapping {}: expected three colon-separated fields", arg),
                format!("{}", err));
        }
    }

    #[test]
    fn test_parse_mappings_bad_type() {
        let args = ["rr:/foo:/bar"];
        let err = parse_mappings(&args).unwrap_err();
        assert_eq!("bad mapping rr:/foo:/bar: type was rr but should be ro or rw",
            format!("{}", err));
    }

    #[test]
    fn test_parse_mappings_bad_path() {
        let args = ["ro:foo:/bar"];
        let err = parse_mappings(&args).unwrap_err();
        assert_eq!("bad mapping ro:foo:/bar: path \"foo\" is not absolute", format!("{}", err));
    }

    #[test]
    fn test_parse_mappings_bad_underlying_path() {
        let args = ["ro:/foo:bar"];
        let err = parse_mappings(&args).unwrap_err();
        assert_eq!("bad mapping ro:/foo:bar: path \"bar\" is not absolute", format!("{}", err));
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
