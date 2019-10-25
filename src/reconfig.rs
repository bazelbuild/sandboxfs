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

use {flatten_causes, Mapping, MappingError};
use failure::Fallible;
use nix::unistd;
use serde_derive::{Deserialize, Serialize};
use std::fs;
use std::io::{self, Read, Write};
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::path::{self, Path, PathBuf};

/// A shareable view into a reconfigurable file system.
pub trait ReconfigurableFS {
    /// Creates a new top-level directory named `id` and applies all given `mappings` within it.
    ///
    /// The paths in `mappings` are specified as absolute, but they are all joined with the name
    /// in `id`.
    fn create_sandbox(&self, id: &str, mappings: &[Mapping]) -> Fallible<()>;

    /// Destroys the top-level directory named `id`.
    fn destroy_sandbox(&self, id: &str) -> Fallible<()>;
}

/// External representation of a mapping in the JSON reconfiguration data.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct JsonMapping {
    path: PathBuf,
    underlying_path: PathBuf,
    writable: bool,
}

/// External representation of a reconfiguration map request.
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
struct CreateSandboxRequest {
    id: String,
    mappings: Vec<JsonMapping>,
}

/// External representation of a reconfiguration request.
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
enum Request {
    CreateSandbox(CreateSandboxRequest),
    DestroySandbox(String),
}

/// External representation of a response to a reconfiguration request.
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
struct Response {
    /// Identifier of the sandbox this response corresponds to.  Not present if the response
    /// corresponds to an unrecoverable error (e.g. a syntax error in the requests stream).
    id: Option<String>,

    /// Contains the error, if any, for a failed reconfiguration request.
    error: Option<String>,
}

/// Checks that a sandbox identifier is valid.
// TODO(jmmv): Should be part of the `Request` deserialization.
// See: https://github.com/serde-rs/serde/issues/642.
fn validate_id(id: &str) -> Fallible<()> {
    if id.is_empty() {
        Err(format_err!("Identifier cannot be empty"))
    } else if id.contains(path::MAIN_SEPARATOR) {
        Err(format_err!("Identifier {} is not a basename", id))
    } else {
        Ok(())
    }
}

/// Joins the identifier of a sandbox in a mapping request with a mapping-specific path from that
/// request.
pub fn make_path<P: AsRef<Path>>(id: &str, abs_path: P) -> Fallible<PathBuf> {
    let abs_path = abs_path.as_ref();
    if !abs_path.is_absolute() {
        return Err(MappingError::PathNotAbsolute { path: abs_path.to_owned() }.into());
    }

    let rel_path = abs_path.strip_prefix("/").unwrap();
    if rel_path.as_os_str().is_empty() {
        Ok(PathBuf::from("/").join(id))
    } else {
        Ok(PathBuf::from("/").join(id).join(rel_path))
    }
}

/// Applies a reconfiguration request to the given file system.
fn handle_request<F: ReconfigurableFS>(request: Request, fs: &F) -> Fallible<()> {
    match request {
        Request::CreateSandbox(request) => {
            validate_id(&request.id)?;
            // TODO(jmmv): This conversion from JsonMapping to Mapping is wasteful and only
            // exists to validate that the mappings' paths are valid.  We should be able to do
            // that as part of the actual parsing of the JSON input instead.
            let mut mappings = Vec::with_capacity(request.mappings.len());
            for mapping in request.mappings {
                mappings.push(Mapping::from_parts(
                    mapping.path, mapping.underlying_path, mapping.writable)?);
            }
            Ok(fs.create_sandbox(&request.id, &mappings)?)
        },
        Request::DestroySandbox(id) => {
            validate_id(&id)?;
            Ok(fs.destroy_sandbox(&id)?)
        },
    }
}

/// Responds to a reconfiguration request with the details contained in a result object.
fn respond(writer: &mut impl Write, id: Option<String>, result: Fallible<()>) -> Fallible<()> {
    let response = Response {
        id: id,
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
/// Writes reconfiguration responses to `output`, which either acknowledge the request or contain
/// details about any semantical errors that occur during the process.
pub fn run_loop(reader: impl Read, writer: impl Write, fs: &impl ReconfigurableFS) -> Fallible<()> {
    let mut reader = io::BufReader::new(reader);
    let mut writer = io::BufWriter::new(writer);
    let mut stream = serde_json::Deserializer::from_reader(&mut reader).into_iter::<Request>();

    loop {
        match stream.next() {
            Some(Ok(request)) => {
                let id = match &request {
                    Request::CreateSandbox(request) => request.id.clone(),
                    Request::DestroySandbox(id) => id.clone(),
                };
                respond(&mut writer, Some(id), handle_request(request, fs))?
            },
            Some(Err(e)) => {
                assert!(!e.is_eof());  // Handled below.
                let result = Err(format_err!("{}", e));
                respond(&mut writer, None, Err(e.into()))?;
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
    fn new_mapping<P: AsRef<Path>>(path: P, underlying_path: P, writable: bool) -> JsonMapping {
        JsonMapping {
            path: PathBuf::from(path.as_ref()),
            underlying_path: PathBuf::from(underlying_path.as_ref()),
            writable: writable,
        }
    }

    /// Syntactic sugar to instantiate a new `Request::CreateSandbox` for testing purposes only.
    fn new_create_sandbox(id: &str, mappings: &[JsonMapping]) -> Request {
        Request::CreateSandbox(
            CreateSandboxRequest {
                id: id.to_owned(),
                mappings: mappings.to_owned(),
            }
        )
    }

    /// Syntactic sugar to instantiate a new `Request::DestroySandbox` for testing purposes only.
    fn new_destroy_sandbox(id: &str) -> Request {
        Request::DestroySandbox(id.to_owned())
    }

    #[test]
    fn test_validate_id() {
        validate_id(&"foo").unwrap();

        assert_eq!(
            "Identifier cannot be empty",
            format!("{}", validate_id(&"").unwrap_err()));

        assert_eq!(
            "Identifier /abs is not a basename",
            format!("{}", validate_id(&"/abs").unwrap_err()));

        assert_eq!(
            "Identifier a/b is not a basename",
            format!("{}", validate_id(&"a/b").unwrap_err()));
    }

    #[test]
    fn test_make_path() {
        assert_eq!(PathBuf::from("/abc"), make_path(&"abc", &"/").unwrap());
        assert_eq!(PathBuf::from("/a/b"), make_path(&"a", &"/b").unwrap());

        assert_eq!(
            "/no/trailing/slash",
            make_path(&"no", &"/trailing/slash//").unwrap().to_str().unwrap());

        assert_eq!(
            format!("{}", MappingError::PathNotAbsolute { path: PathBuf::from("") }),
            format!("{}", make_path(&"empty", &"").unwrap_err()));

        assert_eq!(
            format!("{}", MappingError::PathNotAbsolute { path: PathBuf::from("sub/path") }),
            format!("{}", make_path(&"a", &"sub/path").unwrap_err()));
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
        fn create_sandbox(&self, id: &str, mappings: &[Mapping]) -> Fallible<()> {
            for mapping in mappings {
                let path = make_path(id, &mapping.path).unwrap();
                self.log.lock().unwrap().push(format!("map {}", path.display()));
            }
            Ok(())
        }

        fn destroy_sandbox(&self, id: &str) -> Fallible<()> {
            self.log.lock().unwrap().push(format!("unmap /{}", id));
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
    fn do_run_loop_test(requests: &[Request], exp_responses: &[Response], exp_log: &[String]) {
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
        let requests: &[Request] = &[
            new_create_sandbox("foo", &[new_mapping("/bar", "/bin", false)]),
        ];
        let exp_responses = &[
            Response{ id: Some("foo".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /foo/bar"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_many() {
        let requests: &[Request] = &[
            new_create_sandbox("foo", &[new_mapping("/bar", "/bin", false)]),
            new_destroy_sandbox("a"),
            new_create_sandbox("baz", &[new_mapping("/z", "/b", false)]),
        ];
        let exp_responses = &[
            Response{ id: Some("foo".to_owned()), error: None },
            Response{ id: Some("a".to_owned()), error: None },
            Response{ id: Some("baz".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /foo/bar"),
            String::from("unmap /a"),
            String::from("map /baz/z"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_roots() {
        let requests: &[Request] = &[
            new_create_sandbox("sandbox", &[
                new_mapping("/", "/the-root", false),
                new_mapping("/foo/bar", "/bin", false),
            ]),
            new_destroy_sandbox("somewhere-else"),
        ];
        let exp_responses = &[
            Response{ id: Some("sandbox".to_owned()), error: None },
            Response{ id: Some("somewhere-else".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /sandbox"),
            String::from("map /sandbox/foo/bar"),
            String::from("unmap /somewhere-else"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_errors() {
        let requests: &[Request] = &[
            new_create_sandbox("a", &[new_mapping("/foo", "/b", false)]),
            new_create_sandbox("a", &[new_mapping("bar", "/b", false)]),
            new_create_sandbox("a", &[new_mapping("/z", "/b", false)]),
        ];
        let exp_responses = &[
            Response{ id: Some("a".to_owned()), error: None },
            Response{ id: Some("a".to_owned()), error: Some("\"bar\" is not absolute".to_owned()) },
            Response{ id: Some("a".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /a/foo"),
            String::from("map /a/z"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_fatal_syntax_error_due_to_empty_request() {
        let requests = r#"{}"#;
        let exp_responses = &[
            Response{ id: None, error: Some("expected value".to_string()) },
        ];
        do_run_loop_raw_test(&requests, exp_responses, &[]).unwrap_err();
    }

    #[test]
    fn test_run_loop_fatal_syntax_error_due_to_conflicting_requests() {
        let requests = r#"
            {
                "CreateSandbox": {
                    "id": "foo",
                    "mappings": [{"path": "/foo", "underlying_path": "/foo", "writable": false}]
                },
                "DestroySandbox": "bar"
            }
        "#;
        let exp_responses = &[
            Response{ id: None, error: Some("expected value".to_string()) },
        ];
        do_run_loop_raw_test(&requests, exp_responses, &[]).unwrap_err();
    }

    #[test]
    fn test_run_loop_fatal_syntax_error_stops_processing() {
        let requests = r#"
            {"DestroySandbox": "first"}
            {"CreateSandbox": {
                "root": "second",
                "mappings": [{"path": "/bar"}]
            }}
            {"DestroySandbox": "third"}
        "#;
        let exp_responses = &[
            Response{ id: Some("first".to_owned()), error: None },
            Response{ id: None, error: Some("missing field".to_string()) },
        ];
        let exp_log = &[
            String::from("unmap /first"),
        ];
        do_run_loop_raw_test(&requests, exp_responses, exp_log).unwrap_err();
    }
}
