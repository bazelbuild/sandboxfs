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

use {flatten_causes, Mapping};
use failure::Fallible;
use nix::unistd;
use serde_derive::{Deserialize, Serialize};
use std::fs;
use std::io::{self, Read, Write};
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::path::{Path, PathBuf};

/// A shareable view into a reconfigurable file system.
pub trait ReconfigurableFS {
    /// Maps a path into the file system.
    fn map(&self, mapping: &Mapping) -> Fallible<()>;

    /// Unmaps a path from the file system.
    fn unmap<P: AsRef<Path>>(&self, path: P) -> Fallible<()>;
}

/// External representation of a mapping in the JSON reconfiguration data.
///
/// This exists mostly because the original implementation of sandboxfs was in Go and the internal
/// field names used in its structures leaked to the JSON format.  We must handle those same names
/// here for drop-in compatibility.
#[allow(non_snake_case)]
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct JsonMapping {
    Mapping: PathBuf,
    Target: PathBuf,
    Writable: bool,
}

/// External representation of a reconfiguration step in the JSON reconfiguration data.
///
/// This exists to wrap the `JsonMapping` artifact and for the same reasons described there.
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
enum JsonStep {
    Map(JsonMapping),
    Unmap(PathBuf),
}

/// External representation of a response to a reconfiguration request.
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
struct Response {
    /// Contains the error, if any, for a failed reconfiguration request.
    error: Option<String>,
}

/// Applies a reconfiguration request to the given file system.
fn handle_request<F: ReconfigurableFS>(steps: Vec<JsonStep>, fs: &F) -> Fallible<()> {
    for step in steps {
        match step {
            JsonStep::Map(mapping) => {
                let mapping = Mapping::from_parts(
                    mapping.Mapping, mapping.Target, mapping.Writable)?;
                fs.map(&mapping)?
            },
            JsonStep::Unmap(path) => fs.unmap(&path)?,
        }
    }
    Ok(())
}

/// Responds to a reconfiguration request with the details contained in a result object.
fn respond(writer: &mut impl Write, result: &Fallible<()>) -> Fallible<()> {
    let response = Response {
        error: result.as_ref().err().map(|e| flatten_causes(&e)),
    };
    serde_json::to_writer(writer.by_ref(), &response)?;
    writer.write_all(b"\n")?;
    writer.flush()?;
    Ok(())
}

/// Runs the reconfiguration loop on the given file system `fs`.
///
/// The reconfiguration loop terminates under these conditions:
/// * there is no more input in `reader`, which denotes that the user froze the configuration),
/// * once the the input is closed by a different thread (returning success), or
/// * once parsing stops abruptly due to an error in the input stream (returning such details).
///
/// Writes reconfiguration responses to `output`, which either acknolwedge the request or contain
/// details about any semantical errors that occur during the process.
pub fn run_loop(reader: impl Read, writer: impl Write, fs: &impl ReconfigurableFS) -> Fallible<()> {
    let mut reader = io::BufReader::new(reader);
    let mut writer = io::BufWriter::new(writer);
    let mut stream =
        serde_json::Deserializer::from_reader(&mut reader).into_iter::<Vec<JsonStep>>();

    loop {
        match stream.next() {
            Some(Ok(steps)) => respond(&mut writer, &handle_request(steps, fs))?,
            Some(Err(e)) => {
                assert!(!e.is_eof());  // Handled below.
                let result = Err(e.into());
                respond(&mut writer, &result)?;
                // Parsing failed due to invalid JSON data.  Would be nice to recover from this by
                // advancing the stream to the next valid request, but this is currently not
                // possible; see https://github.com/serde-rs/json/issues/70.
                return result;
            },
            None => {
                return Ok(());
            }
        };
    }
}

/// Opens the input file for the reconfiguration loop.
///
/// If `path` is None, this reopens stdin.
#[allow(unsafe_code)]
pub fn open_input<P: AsRef<Path>>(path: Option<P>) -> Fallible<fs::File> {
    match path.as_ref() {
        Some(path) => Ok(fs::File::open(&path)?),
        None => unsafe {
            Ok(fs::File::from_raw_fd(unistd::dup(io::stdin().as_raw_fd())?))
        },
    }
}

/// Opens the output file for the reconfiguration loop.
///
/// If `path` is None, this reopen stdout.
#[allow(unsafe_code)]
pub fn open_output<P: AsRef<Path>>(path: Option<P>) -> Fallible<fs::File> {
    match path.as_ref() {
        Some(path) => Ok(fs::OpenOptions::new().write(true).open(&path)?),
        None => unsafe {
            Ok(fs::File::from_raw_fd(unistd::dup(io::stdout().as_raw_fd())?))
        },
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::sync::Mutex;
    use super::*;

    /// Syntactic sugar to instantiate a new `JsonStep::Map` for testing purposes only.
    fn new_map_step<P: AsRef<Path>>(path: P, underlying_path: P, writable: bool) -> JsonStep {
        JsonStep::Map(JsonMapping {
            Mapping: PathBuf::from(path.as_ref()),
            Target: PathBuf::from(underlying_path.as_ref()),
            Writable: writable,
        })
    }

    /// Syntactic sugar to instantiate a new `JsonStep::Unmap` for testing purposes only.
    fn new_unmap_step<P: AsRef<Path>>(path: P) -> JsonStep {
        JsonStep::Unmap(PathBuf::from(path.as_ref()))
    }

    /// A reconfigurable file system that tracks map and unmap operations.
    #[derive(Default)]
    struct MockFS {
        /// Capture of all reconfiguration requests received, in order.
        ///
        /// Map operations are recorded as "map foo" and unmap operations are recorded as "unmap
        /// foo", where "foo" is the path of the mapping.
        log: Mutex<Vec<String>>,
    }

    impl MockFS {
        /// Returns a copy of the operations log.
        fn get_log(&self) -> Vec<String> {
            self.log.lock().unwrap().to_owned()
        }
    }

    impl ReconfigurableFS for MockFS {
        fn map(&self, mapping: &Mapping) -> Fallible<()> {
            self.log.lock().unwrap().push(format!("map {}", mapping.path.display()));
            Ok(())
        }

        fn unmap<P: AsRef<Path>>(&self, path: P) -> Fallible<()> {
            self.log.lock().unwrap().push(format!("unmap {}", path.as_ref().display()));
            Ok(())
        }
    }

    /// A `Response` that matches another `Response`'s error message in a fuzzy manner.
    ///
    /// To be used for testing purposes only to prevent recording exact error messages in the
    /// tests.
    #[derive(Debug)]
    struct FuzzyResponse<'a>(&'a Response);

    impl <'a> From<&'a Response> for FuzzyResponse<'a> {
        /// Constructs a new `FuzzyResponse` adapter from a `Response`.
        fn from(response: &'a Response) -> Self {
            Self(response)
        }
    }

    impl <'a> PartialEq<Response> for FuzzyResponse<'a> {
        /// Checks if the `FuzzyResponse` matches a given `Response`.
        ///
        /// The two are considered equivalent if the pattern provided in the `FuzzyResponse`'s
        /// error message matches the error message in `other`.
        fn eq(&self, other: &Response) -> bool {
            match (self.0.error.as_ref(), other.error.as_ref()) {
                (Some(exp_message), Some(message)) => message.contains(exp_message),
                (None, None) => true,
                (_, _) => false,
            }
        }
    }

    /// Executes `run_loop` given a raw JSON request and expects the set of responses in
    /// `exp_responses` and the set of side-effects on a file system in `exp_log`.
    ///
    /// Returns the result of `run_loop` (be it successful or failure) *after* all other conditions
    /// have been checked.
    fn do_run_loop_raw_test(requests: &str, exp_responses: &[Response], exp_log: &[String])
        -> Fallible<()> {
        let fs: MockFS = Default::default();

        let reader = io::BufReader::new(requests.as_bytes());

        let mut output: Vec<u8> = Vec::new();
        let writer = io::BufWriter::new(&mut output);

        let result = run_loop(reader, writer, &fs);

        let mut output: io::BufReader<&[u8]> = io::BufReader::new(output.as_ref());
        let stream = serde_json::Deserializer::from_reader(&mut output).into_iter::<Response>();
        let mut responses = vec!();
        for response in stream {
            responses.push(response.unwrap());
        }

        let exp_responses: Vec<FuzzyResponse> =
            exp_responses.iter().map(FuzzyResponse::from).collect();
        assert_eq!(exp_responses, responses.as_slice());
        assert_eq!(exp_log, fs.get_log().as_slice());

        result
    }

    /// Executes `run_loop` given a collection of requests and expects the set of responses in
    /// `exp_responses` and the set of side-effects on a file system in `exp_log`.
    ///
    /// This expects `run_loop` to complete successfully (even if individual requests fail in a
    /// recoverable manner).  To check for the behavior of this function under fatal erroneous
    /// conditions, use `do_run_loop_raw_test`.
    fn do_run_loop_test(requests: &[&[JsonStep]], exp_responses: &[Response], exp_log: &[String]) {
        let mut input = String::new();
        for request in requests {
            input += &serde_json::to_string(&request).unwrap();
        }
        do_run_loop_raw_test(&input, exp_responses, exp_log).unwrap();
    }

    #[test]
    fn test_run_loop_empty() {
        do_run_loop_test(&[], &[], &[]);
    }

    #[test]
    fn test_run_loop_one() {
        let requests: &[&[JsonStep]] = &[
            &[new_map_step("/foo/bar", "/bin", false)],
        ];
        let exp_responses = &[
            Response{ error: None },
        ];
        let exp_log = &[
            String::from("map /foo/bar"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_many() {
        let requests: &[&[JsonStep]] = &[
            &[new_map_step("/foo/bar", "/bin", false), new_unmap_step("/a")],
            &[new_map_step("/z", "/b", false)],
        ];
        let exp_responses = &[
            Response{ error: None },
            Response{ error: None },
        ];
        let exp_log = &[
            String::from("map /foo/bar"),
            String::from("unmap /a"),
            String::from("map /z"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_errors() {
        let requests: &[&[JsonStep]] = &[
            &[new_map_step("/foo", "/bin", false)],
            &[new_map_step("bar", "/b", false)],
            &[new_map_step("/z", "/b", false)],
        ];
        let exp_responses = &[
            Response{ error: None },
            Response{ error: Some("path \"bar\" is not absolute".to_owned()) },
            Response{ error: None },
        ];
        let exp_log = &[
            String::from("map /foo"),
            String::from("map /z"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_fatal_syntax_error_due_to_empty_request() {
        let requests = r#"[{}]"#;
        let exp_responses = &[
            Response{ error: Some("expected value".to_string()) },
        ];
        do_run_loop_raw_test(&requests, exp_responses, &[]).unwrap_err();
    }

    #[test]
    fn test_run_loop_fatal_syntax_error_due_to_conflicting_requests() {
        let requests = r#"
            [{"Map": {"Mapping": "/foo", "Target": "%ROOT%", "Writable": false}, "Unmap": "/bar"}]
        "#;
        let exp_responses = &[
            Response{ error: Some("expected value".to_string()) },
        ];
        do_run_loop_raw_test(&requests, exp_responses, &[]).unwrap_err();
    }

    #[test]
    fn test_run_loop_fatal_syntax_error_stops_processing() {
        let requests = r#"
            [{"Unmap": "/foo"}]
            [{"Map": {"Mapping": "/bar"}}]
            [{"Unmap": "/baz"}]
        "#;
        let exp_responses = &[
            Response{ error: None },
            Response{ error: Some("missing field".to_string()) },
        ];
        let exp_log = &[
            String::from("unmap /foo"),
        ];
        do_run_loop_raw_test(&requests, exp_responses, exp_log).unwrap_err();
    }
}
