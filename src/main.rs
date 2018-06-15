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
extern crate getopts;
extern crate sandboxfs;

use getopts::Options;
use std::env;
use std::ffi::OsStr;
use std::io;
use std::path::Path;
use std::process;
use std::result::Result;

/// Error-like type to encapsulate various different error conditions in the
/// main program.
#[derive(Debug)]
enum MainError {
    /// Execution failed due to a generic runtime error.
    Runtime(String),
    /// Execution failed due to a user-triggered error.
    Usage(String),
}

impl From<io::Error> for MainError {
    fn from(err: io::Error) -> Self {
        MainError::Runtime(format!("{}", err))
    }
}

impl From<getopts::Fail> for MainError {
    fn from(err: getopts::Fail) -> Self {
        MainError::Usage(format!("{}", err))
    }
}

/// Obtains the program name from the execution's first argument, or returns a
/// default if the program name cannot be determined for whatever reason.
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

/// Program's entry point.  This is a "safe" version of `main` in the sense that
/// this doesn't directly handle errors: all errors are returned to the caller
/// for consistent reporter to the user depending on their type.
fn safe_main(program: &str, args: &[String]) -> Result<(), MainError> {
    env_logger::init();

    let mut opts = Options::new();
    opts.optflag("", "help", "prints usage information and exits");
    let matches = opts.parse(args)?;

    if matches.opt_present("help") {
        usage(&program, &opts);
        return Ok(());
    }

    let mount_point = if matches.free.len() == 1 {
        &matches.free[0]
    } else {
        return Err(MainError::Usage("invalid number of arguments".to_string()));
    };

    sandboxfs::mount(Path::new(mount_point))?;
    Ok(())
}

/// Program's entry point.  This delegates to `safe_main` for all program logic
/// and is just in charge of consistently formatting and reporting all possible
/// errors to the caller.
fn main() {
    let args: Vec<String> = env::args().collect();
    let program = program_name(&args, "sandboxfs");

    match safe_main(&program, &args[1..]) {
        Ok(_) => (),
        Err(MainError::Runtime(message)) => {
            eprintln!("{}: {}", program, message);
            process::exit(1);
        },
        Err(MainError::Usage(message)) => {
            eprintln!("Usage error: {}", message);
            eprintln!("Type {} --help for more information", program);
            process::exit(2);
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
