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

use {Mapping, MappingError};
use errors::flatten_causes;
use failure::{Fallible, ResultExt};
use nix::unistd;
use serde_derive::{Deserialize, Serialize};
use std::collections::HashMap;
use std::collections::hash_map::Entry;
use std::fs;
use std::io::{self, Read, Write};
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::path::{self, Path, PathBuf};
use std::sync::{Arc, Mutex};
use threadpool::ThreadPool;

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
    #[serde(alias = "x", default)]
    path_prefix: u32,

    #[serde(alias = "p")]
    path: String,

    #[serde(alias = "y", default)]
    underlying_path_prefix: u32,

    #[serde(alias = "u")]
    underlying_path: String,

    #[serde(alias = "w", default)]
    writable: bool,
}

/// External representation of a reconfiguration map request.
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
struct CreateSandboxRequest {
    #[serde(alias = "i")]
    id: String,

    #[serde(alias = "m", default)]
    mappings: Vec<JsonMapping>,

    // The keys here should be u32s but JSON doesn't support non-string object keys.
    #[serde(alias = "q", default)]
    prefixes: HashMap<String, PathBuf>,
}

/// External representation of a reconfiguration request.
#[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
enum Request {
    #[serde(alias = "C")]
    CreateSandbox(CreateSandboxRequest),

    #[serde(alias = "D")]
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

/// Tracks prefixes seen in the requests to handle the prefix-encoded paths.
#[derive(Debug)]
struct Prefixes {
    /// Mapping of prefix identifier to the path for the prefix.
    data: HashMap<u32, PathBuf>,
}

impl Prefixes {
    /// Construct an empty set of prefixes.
    ///
    /// The prefix with identifier `0` is reserved and allows specifying paths as absolute instead
    /// of path-encoded for readability purposes.
    fn new() -> Self {
        let mut data = HashMap::new();
        data.insert(0, PathBuf::from(""));
        Self { data }
    }

    /// Registers all new path prefixes that appear in the `request` and returns a new view that
    /// only contains the prefixes needed to process the request.
    fn register(&mut self, request: &Request) -> Fallible<Prefixes> {
        let mut used_prefixes: HashMap<u32, PathBuf> = HashMap::new();

        if let Request::CreateSandbox(request) = &request {
            let mut count = 0;
            for (id, path) in &request.prefixes {
                let id = id.parse::<u32>().context("Bad prefix number")?;
                match self.data.entry(id) {
                    Entry::Occupied(e) => {
                        let previous_path = e.get();
                        if previous_path != path {
                            return Err(format_err!("Prefix {} already had path {} but got new {}",
                                id, previous_path.display(), path.display()));
                        }
                    },
                    Entry::Vacant(e) => {
                        e.insert(path.clone());
                        count += 1;
                    },
                };
            }
            if count > 0 {
                info!("Registered {} new prefixes", count);
            }

            for m in &request.mappings {
                match self.data.get(&m.path_prefix) {
                    Some(prefix) => used_prefixes.entry(m.path_prefix)
                        .or_insert_with(|| prefix.clone()),
                    None => return Err(format_err!("Prefix {} does not exist", m.path_prefix)),
                };

                match self.data.get(&m.underlying_path_prefix) {
                    Some(prefix) => used_prefixes.entry(m.underlying_path_prefix)
                        .or_insert_with(|| prefix.clone()),
                    None => return
                        Err(format_err!("Prefix {} does not exist", m.underlying_path_prefix)),
                };
            }
        }

        Ok(Prefixes { data: used_prefixes })
    }

    /// Builds a path given a `prefix` (which must exist) and an arbitrary `suffix`.
    fn build_path(&self, prefix: u32, suffix: &str) -> Fallible<PathBuf> {
        if prefix != 0 && suffix.starts_with('/') {
            return Err(format_err!("Suffix {} must be relative", suffix));
        }
        let prefix = self.data.get(&prefix).expect("Prefix existence validated in register()");
        if suffix.is_empty() {
            Ok(prefix.clone())
        } else {
            let path = prefix.join(suffix);
            debug_assert!(path.starts_with(prefix), "Suffix was not relative");
            Ok(path)
        }
    }
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
fn handle_request<F: ReconfigurableFS>(request: Request, fs: &F, prefixes: Fallible<Prefixes>)
    -> Fallible<()> {
    let prefixes = &prefixes?;  // Unwrap any possible error as part of this request.
    match request {
        Request::CreateSandbox(request) => {
            validate_id(&request.id)?;
            let mut mappings = Vec::with_capacity(request.mappings.len());
            for mapping in request.mappings {
                let path = prefixes.build_path(mapping.path_prefix, &mapping.path)?;
                let underlying_path = prefixes.build_path(mapping.underlying_path_prefix,
                    &mapping.underlying_path)?;
                mappings.push(Mapping::from_parts(path, underlying_path, mapping.writable)?);
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
fn respond(writer: Arc<Mutex<io::BufWriter<impl Write>>>, id: Option<String>, result: Fallible<()>)
    -> Fallible<()> {
    let mut writer = writer.lock().unwrap();
    let response = Response {
        id: id,
        error: result.as_ref().err().map(|e| flatten_causes(&e)),
    };
    serde_json::to_writer(writer.by_ref(), &response)?;
    writer.write_all(b"\n")?;
    writer.flush()?;
    Ok(())
}

/// Same as `run_loop` but takes a thread pool instead of a number of threads.
///
/// This is a separate function to ensure we control the lifecycle of the thread pool on the caller
/// side, as `ThreadPool` does not call `join` when dropped.
fn run_loop_aux(
    reader: impl Read,
    writer: impl Write + Send + Sync + 'static,
    pool: &ThreadPool,
    fs: &(impl ReconfigurableFS + Send + Sync + Clone + 'static))
    -> Fallible<()> {

    let mut reader = io::BufReader::new(reader);
    let writer = Arc::from(Mutex::from(io::BufWriter::new(writer)));
    let mut stream = serde_json::Deserializer::from_reader(&mut reader).into_iter::<Request>();

    let mut prefixes = Prefixes::new();

    loop {
        let writer = writer.clone();
        match stream.next() {
            Some(Ok(request)) => {
                let fs = fs.clone();
                let used_prefixes = prefixes.register(&request);
                pool.execute(move || {
                    let id = match &request {
                        Request::CreateSandbox(request) => request.id.clone(),
                        Request::DestroySandbox(id) => id.clone(),
                    };
                    let result = handle_request(request, &fs, used_prefixes);
                    if let Err(e) = respond(writer, Some(id), result) {
                        warn!("Failed to write response: {}", e);
                    }
                });
            },
            Some(Err(e)) => {
                assert!(!e.is_eof());  // Handled below.
                let result = Err(format_err!("{}", e));
                respond(writer, None, Err(e.into()))?;
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

/// Runs the reconfiguration loop on the given file system `fs`.
///
/// The reconfiguration loop terminates under these conditions:
/// * there is no more input in `reader`, which denotes that the user froze the configuration,
/// * once the the input is closed by a different thread (returning success), or
/// * once parsing stops abruptly due to an error in the input stream (returning such details).
///
/// Writes reconfiguration responses to `output`, which either acknowledge the request or contain
/// details about any semantical errors that occur during the process.
///
/// The reconfiguration loop is configured to accept `threads` parallel requests.
pub fn run_loop(
    reader: impl Read,
    writer: impl Write + Send + Sync + 'static,
    threads: usize,
    fs: &(impl ReconfigurableFS + Send + Sync + Clone + 'static))
    -> Fallible<()> {

    info!("Using {} threads for reconfigurations", threads);
    let pool = ThreadPool::new(threads);
    let result = run_loop_aux(reader, writer, &pool, fs);
    pool.join();
    result
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
    use std::collections::HashMap;
    use std::io::Seek;
    use std::sync::Mutex;
    use super::*;
    use tempfile;

    /// Syntactic sugar to instantiate a new `JsonStep::Map` for testing purposes only.
    fn new_mapping(path: &str, path_prefix: u32, underlying_path: &str, underlying_path_prefix: u32,
        writable: bool) -> JsonMapping {
        JsonMapping {
            path: path.to_owned(),
            path_prefix: path_prefix,
            underlying_path: underlying_path.to_owned(),
            underlying_path_prefix: underlying_path_prefix,
            writable: writable,
        }
    }

    /// Syntactic sugar to instantiate a new `Request::CreateSandbox` for testing purposes only.
    fn new_create_sandbox(id: &str, mappings: &[JsonMapping], prefixes: HashMap<String, PathBuf>)
        -> Request {
        Request::CreateSandbox(
            CreateSandboxRequest {
                id: id.to_owned(),
                mappings: mappings.to_owned(),
                prefixes: prefixes,
            }
        )
    }

    /// Syntactic sugar to instantiate a new `Request::DestroySandbox` for testing purposes only.
    fn new_destroy_sandbox(id: &str) -> Request {
        Request::DestroySandbox(id.to_owned())
    }

    #[test]
    fn test_prefixes_create_one_sandbox() {
        let mut prefixes = Prefixes::new();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("1".to_owned(), PathBuf::from("/"));
        request_prefixes.insert("2".to_owned(), PathBuf::from("/some/dir"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 1, "bar", 1, false),
            new_mapping("another", 2, "bar", 1, false),
        ], request_prefixes);

        let subset = prefixes.register(&request).unwrap();
        assert_eq!(3, prefixes.data.len());
        assert_eq!(&PathBuf::from(""), prefixes.data.get(&0).unwrap());
        assert_eq!(&PathBuf::from("/"), prefixes.data.get(&1).unwrap());
        assert_eq!(&PathBuf::from("/some/dir"), prefixes.data.get(&2).unwrap());
        assert_eq!(2, subset.data.len());
        assert_eq!(&PathBuf::from("/"), subset.data.get(&1).unwrap());
        assert_eq!(&PathBuf::from("/some/dir"), subset.data.get(&2).unwrap());
    }

    #[test]
    fn test_prefixes_create_two_disjoint_sandboxes() {
        let mut prefixes = Prefixes::new();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("1".to_owned(), PathBuf::from("/"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 1, "bar", 1, false),
        ], request_prefixes);

        let subset = prefixes.register(&request).unwrap();
        assert_eq!(2, prefixes.data.len());
        assert_eq!(&PathBuf::from(""), prefixes.data.get(&0).unwrap());
        assert_eq!(&PathBuf::from("/"), prefixes.data.get(&1).unwrap());
        assert_eq!(1, subset.data.len());
        assert_eq!(&PathBuf::from("/"), subset.data.get(&1).unwrap());

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("2".to_owned(), PathBuf::from("/other"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 2, "bar", 2, false),
        ], request_prefixes);

        let subset = prefixes.register(&request).unwrap();
        assert_eq!(3, prefixes.data.len());
        assert_eq!(&PathBuf::from(""), prefixes.data.get(&0).unwrap());
        assert_eq!(&PathBuf::from("/"), prefixes.data.get(&1).unwrap());
        assert_eq!(&PathBuf::from("/other"), prefixes.data.get(&2).unwrap());
        assert_eq!(1, subset.data.len());
        assert_eq!(&PathBuf::from("/other"), subset.data.get(&2).unwrap());
    }

    #[test]
    fn test_prefixes_create_two_sandboxes_with_common_prefixes() {
        let mut prefixes = Prefixes::new();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("1".to_owned(), PathBuf::from("/first"));
        request_prefixes.insert("3".to_owned(), PathBuf::from("/third"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 3, "bar", 1, false),
        ], request_prefixes);

        let subset = prefixes.register(&request).unwrap();
        assert_eq!(3, prefixes.data.len());
        assert_eq!(&PathBuf::from(""), prefixes.data.get(&0).unwrap());
        assert_eq!(&PathBuf::from("/first"), prefixes.data.get(&1).unwrap());
        assert_eq!(&PathBuf::from("/third"), prefixes.data.get(&3).unwrap());
        assert_eq!(2, subset.data.len());
        assert_eq!(&PathBuf::from("/first"), subset.data.get(&1).unwrap());
        assert_eq!(&PathBuf::from("/third"), subset.data.get(&3).unwrap());

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("2".to_owned(), PathBuf::from("/second"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 2, "bar", 3, false),
        ], request_prefixes);

        let subset = prefixes.register(&request).unwrap();
        assert_eq!(4, prefixes.data.len());
        assert_eq!(&PathBuf::from(""), prefixes.data.get(&0).unwrap());
        assert_eq!(&PathBuf::from("/first"), prefixes.data.get(&1).unwrap());
        assert_eq!(&PathBuf::from("/second"), prefixes.data.get(&2).unwrap());
        assert_eq!(&PathBuf::from("/third"), prefixes.data.get(&3).unwrap());
        assert_eq!(2, subset.data.len());
        assert_eq!(&PathBuf::from("/second"), prefixes.data.get(&2).unwrap());
        assert_eq!(&PathBuf::from("/third"), subset.data.get(&3).unwrap());
    }

    #[test]
    fn test_prefixes_create_two_sandboxes_with_inconsistent_prefixes() {
        let mut prefixes = Prefixes::new();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("1".to_owned(), PathBuf::from("/first"));
        request_prefixes.insert("3".to_owned(), PathBuf::from("/third"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 3, "bar", 1, false),
        ], request_prefixes);

        prefixes.register(&request).unwrap();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("3".to_owned(), PathBuf::from("/other"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 3, "bar", 1, false),
        ], request_prefixes);

        let err = prefixes.register(&request).unwrap_err();
        assert_eq!("Prefix 3 already had path /third but got new /other", format!("{}", err));
    }

    #[test]
    fn test_prefixes_bad_prefix_id() {
        let mut prefixes = Prefixes::new();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("-5".to_owned(), PathBuf::from("/"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 3, "bar", 1, false),
        ], request_prefixes);

        let err = prefixes.register(&request).unwrap_err();
        assert_eq!("Bad prefix number", format!("{}", err));
    }

    #[test]
    fn test_prefixes_unknown_path_prefix() {
        let mut prefixes = Prefixes::new();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("7".to_owned(), PathBuf::from("/"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 5, "bar", 7, false),
        ], request_prefixes);

        let err = prefixes.register(&request).unwrap_err();
        assert_eq!("Prefix 5 does not exist", format!("{}", err));
    }

    #[test]
    fn test_prefixes_unknown_underlying_path_prefix() {
        let mut prefixes = Prefixes::new();

        let mut request_prefixes: HashMap<String, PathBuf> = HashMap::new();
        request_prefixes.insert("7".to_owned(), PathBuf::from("/"));
        let request = new_create_sandbox("irrelevant", &[
            new_mapping("foo", 7, "bar", 9, false),
        ], request_prefixes);

        let err = prefixes.register(&request).unwrap_err();
        assert_eq!("Prefix 9 does not exist", format!("{}", err));
    }

    #[test]
    fn test_prefixes_destroy_sandbox() {
        let mut prefixes = Prefixes::new();
        let subset = prefixes.register(&Request::DestroySandbox("foo".to_owned())).unwrap();
        assert_eq!(1, prefixes.data.len());
        assert_eq!(&PathBuf::from(""), prefixes.data.get(&0).unwrap());
        assert!(subset.data.is_empty());
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
    #[derive(Clone, Default)]
    struct MockFS {
        /// Capture of all reconfiguration requests received, in order.
        ///
        /// Map operations are recorded as "map foo" and unmap operations are recorded as "unmap
        /// foo", where "foo" is the path of the mapping.
        log: Arc<Mutex<Vec<String>>>,
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
                self.log.lock().unwrap().push(
                    format!("map {} -> {}", path.display(), mapping.underlying_path.display()));
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

        // Using a file is the lazy way of getting a concurrently-accessible buffer across the
        // reconfiguration threads.
        let mut file = tempfile::tempfile().unwrap();

        let result = {
            let output = file.try_clone().unwrap();
            let writer = io::BufWriter::new(output);
            run_loop(reader, writer, 1, &fs)
        };

        file.seek(io::SeekFrom::Start(0)).unwrap();
        let mut output = io::BufReader::new(file);
        let stream = serde_json::Deserializer::from_reader(&mut output).into_iter::<Response>();
        let mut responses = HashMap::new();
        for response in stream {
            let response = response.unwrap();
            responses.insert(response.id.clone(), response);
        }

        let exp_responses: HashMap<Option<String>, FuzzyResponse> =
            exp_responses.iter().map(|v| (v.id.clone(), FuzzyResponse::from(v))).collect();
        assert_eq!(exp_responses.len(), responses.len());
        for response in responses {
            assert_eq!(
                exp_responses.get(&response.0).unwrap_or_else(
                    || panic!("No expected response id={:?}, error={:?}", response.0, response.1)),
                &response.1);
        }
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
            new_create_sandbox("foo", &[new_mapping("/bar", 0, "/bin", 0, false)], HashMap::new()),
        ];
        let exp_responses = &[
            Response{ id: Some("foo".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /foo/bar -> /bin"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_many() {
        let requests: &[Request] = &[
            new_create_sandbox("foo", &[new_mapping("/bar", 0, "/bin", 0, false)], HashMap::new()),
            new_destroy_sandbox("a"),
            new_create_sandbox("baz", &[new_mapping("/z", 0, "/b", 0, false)], HashMap::new()),
        ];
        let exp_responses = &[
            Response{ id: Some("foo".to_owned()), error: None },
            Response{ id: Some("a".to_owned()), error: None },
            Response{ id: Some("baz".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /foo/bar -> /bin"),
            String::from("unmap /a"),
            String::from("map /baz/z -> /b"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_roots() {
        let requests: &[Request] = &[
            new_create_sandbox("sandbox", &[
                new_mapping("/", 0, "/the-root", 0, false),
                new_mapping("/foo/bar", 0, "/bin", 0, false),
            ], HashMap::new()),
            new_destroy_sandbox("somewhere-else"),
        ];
        let exp_responses = &[
            Response{ id: Some("sandbox".to_owned()), error: None },
            Response{ id: Some("somewhere-else".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /sandbox -> /the-root"),
            String::from("map /sandbox/foo/bar -> /bin"),
            String::from("unmap /somewhere-else"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_errors() {
        let mut prefixes: HashMap<String, PathBuf> = HashMap::new();
        prefixes.insert("1".to_owned(), PathBuf::from("/"));
        prefixes.insert("2".to_owned(), PathBuf::from(""));
        let requests: &[Request] = &[
            new_create_sandbox("a", &[new_mapping("foo", 1, "b", 1, false)], prefixes),
            new_create_sandbox("a", &[new_mapping("bar", 2, "b", 1, false)], HashMap::new()),
            new_create_sandbox("a", &[new_mapping("z", 1, "b", 1, false)], HashMap::new()),
        ];
        let exp_responses = &[
            Response{ id: Some("a".to_owned()), error: None },
            Response{ id: Some("a".to_owned()), error: Some("\"bar\" is not absolute".to_owned()) },
            Response{ id: Some("a".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /a/foo -> /b"),
            String::from("map /a/z -> /b"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_prefixes_ok() {
        let mut prefixes1: HashMap<String, PathBuf> = HashMap::new();
        prefixes1.insert("1".to_owned(), PathBuf::from("/"));
        prefixes1.insert("2".to_owned(), PathBuf::from("/some/dir"));
        let mut prefixes2: HashMap<String, PathBuf> = HashMap::new();
        prefixes2.insert("3".to_owned(), PathBuf::from("/another/dir"));
        prefixes2.insert("1".to_owned(), PathBuf::from("/"));
        prefixes2.insert("0".to_owned(), PathBuf::from(""));
        let requests: &[Request] = &[
            new_create_sandbox("sandbox1", &[
                new_mapping("", 1, "relative/dir", 2, false),
                new_mapping("/abs/first", 0, "/abs/second", 0, false),
            ], prefixes1),
            new_create_sandbox("sandbox2", &[
                new_mapping("a/b/c", 2, "z", 1, false),
                new_mapping("flat", 2, "x/y", 3, false),
            ], prefixes2),
        ];
        let exp_responses = &[
            Response{ id: Some("sandbox1".to_owned()), error: None },
            Response{ id: Some("sandbox2".to_owned()), error: None },
        ];
        let exp_log = &[
            String::from("map /sandbox1 -> /some/dir/relative/dir"),
            String::from("map /sandbox1/abs/first -> /abs/second"),
            String::from("map /sandbox2/some/dir/a/b/c -> /z"),
            String::from("map /sandbox2/some/dir/flat -> /another/dir/x/y"),
        ];
        do_run_loop_test(requests, exp_responses, exp_log);
    }

    #[test]
    fn test_run_loop_prefixes_errors() {
        let mut prefixes: HashMap<String, PathBuf> = HashMap::new();
        prefixes.insert("1".to_owned(), PathBuf::from("/x"));
        let requests: &[Request] = &[
            new_create_sandbox("a", &[new_mapping("y", 1, "/y", 1, false)], prefixes),
            new_create_sandbox("b", &[new_mapping("y", 0, "z", 1, false)], HashMap::new()),
            new_create_sandbox("c", &[new_mapping("y", 1, "y", 2, false)], HashMap::new()),
            new_create_sandbox("d", &[new_mapping("y", 1, "", 0, false)], HashMap::new()),
        ];
        let exp_responses = &[
            Response{
                id: Some("a".to_owned()), error: Some("Suffix /y must be relative".to_owned()) },
            Response{
                id: Some("b".to_owned()), error: Some("path \"y\" is not absolute".to_owned()) },
            Response{
                id: Some("c".to_owned()), error: Some("Prefix 2 does not exist".to_owned()) },
            Response{
                id: Some("d".to_owned()), error: Some("path \"\" is not absolute".to_owned()) },
        ];
        let exp_log = &[];
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
                    "mappings": [
                        {
                            "path": "foo",
                            "path_prefix": 1,
                            "underlying_path": "foo",
                            "underlying_path_prefix": 1,
                            "writable": false
                        }
                    ],
                    "prefixes": {}
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
