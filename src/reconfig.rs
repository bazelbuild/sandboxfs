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
use failure::Error;
use serde_derive::Deserialize;
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::result::Result;

/// A shareable view into a reconfigurable file system.
pub trait ReconfigurableFS {
    /// Maps a path into the file system.
    fn map(&self, mapping: &Mapping) -> Result<(), Error>;

    /// Unmaps a path from the file system.
    fn unmap<P: AsRef<Path>>(&self, path: P) -> Result<(), Error>;
}

/// External representation of a mapping in the JSON reconfiguration data.
///
/// This exists mostly because the original implementation of sandboxfs was in Go and the internal
/// field names used in its structures leaked to the JSON format.  We must handle those same names
/// here for drop-in compatibility.
#[allow(non_snake_case)]
#[derive(Debug, Deserialize, Eq, PartialEq)]
pub struct JsonMapping {
    Mapping: PathBuf,
    Target: PathBuf,
    Writable: bool,
}

/// External representation of a reconfiguration step in the JSON reconfiguration data.
///
/// This exists to wrap the `JsonMapping` artifact and for the same reasons described there.
#[derive(Debug, Deserialize, Eq, PartialEq)]
enum JsonStep {
    Map(JsonMapping),
    Unmap(PathBuf),
}

/// A valid reconfiguration step.
#[derive(Debug, Eq, PartialEq)]
enum Step {
    /// Indicates that a new path has to be mapped within the sandbox.
    Map(Mapping),

    /// Indicates that a path (and its descendents, if any) has to be unmapped from the sandbox.
    Unmap(PathBuf),
}

/// Extracts a reconfiguration chunk from the input.
///
/// Reconfiguration chunks are delimited by an empty blank line between them or EOF.  Returns `None`
/// after the last chunk has been read and upon encountering EOF.
fn read_chunk<R: io::BufRead>(input: &mut R) -> Result<Option<String>, Error> {
    let mut chunk = String::new();
    loop {
        let n = input.read_line(&mut chunk)?;
        if n == 0 {  // EOF
            if chunk.is_empty() {
                return Ok(None);
            } else {
                return Ok(Some(chunk));
            }
        } else if chunk.ends_with("\n\n") {
            chunk.pop().expect("Chunk should have had two trailing newlines");
            return Ok(Some(chunk));
        }
    }
}

/// Parses the contents of a JSON reconfiguration chunk and returns it as a list of valid steps.
fn parse_chunk(chunk: &str) -> Result<Vec<Step>, Error> {
    let json: Vec<JsonStep> = serde_json::from_str(chunk)?;

    let mut steps: Vec<Step> = Vec::with_capacity(json.len());
    for json_step in json {
        match json_step {
            JsonStep::Map(m) => steps.push(
                Step::Map(Mapping::from_parts(m.Mapping, m.Target, m.Writable)?)),
            JsonStep::Unmap(path) => steps.push(Step::Unmap(path)),
        }
    }
    Ok(steps)
}

/// Reads and handles the next reconfiguration request.
///
/// Returns true upon reaching the end of the input and false if there may be more requests to
/// process.  The caller is responsible for communicating the result of the request into the output
/// writer.
fn handle_request<I: io::BufRead, F: ReconfigurableFS>(reader: &mut I, fs: &F)
    -> Result<bool, Error> {
    match read_chunk(reader)? {
        None => Ok(true),
        Some(chunk) => {
            let steps = parse_chunk(&chunk)?;
            for step in steps {
                match step {
                    Step::Map(mapping) => fs.map(&mapping)?,
                    Step::Unmap(path) => fs.unmap(&path)?,
                }
            }
            Ok(false)
        },
    }
}

/// Runs the reconfiguration loop on the given file system `fs`.
///
/// The reconfiguration loop terminates once there is no more input in `reader` (which denotes that
/// the user froze the configuration), or once the the input is closed by a different thread.
///
/// Writes reconfiguration responses to `output`, which contain details about any error that occurs
/// during the process.
pub fn run_loop(mut reader: io::BufReader<impl Read>, mut writer: io::BufWriter<impl Write>,
    fs: &impl ReconfigurableFS) {
    // TODO(jmmv): This essentially implements an RPC system, and in a rather poor way.  Investigate
    // whether we can replace this with local gRPC; must be careful about the performance
    // implications of that as any changes here can noticeably influence the cost of a sandboxed
    // Bazel build.
    loop {
        match handle_request(&mut reader, fs) {
            Ok(true) => break,  // EOF
            Ok(false) => {
                if let Err(e) = writer.write_fmt(format_args!("Done\n")) {
                    warn!("Failed to write to reconfiguration output: {}", e);
                }
            },
            Err(e) => {
                if let Err(e) = writer.write_fmt(format_args!("Reconfig failed: {}\n",
                    flatten_causes(&e))) {
                    warn!("Failed to write to reconfiguration output: {}", e);
                }
            },
        };
        if let Err(e) = writer.flush() {
            warn!("Failed to flush reconfiguration output: {}", e);
        }
    }
    info!("Reached end of reconfiguration input; file system mappings are now frozen");
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::sync::Mutex;
    use super::*;

    #[test]
    fn test_read_chunk_empty() {
        let mut reader = io::BufReader::new(b"" as &[u8]);
        assert!(read_chunk(&mut reader).unwrap().is_none());
    }

    #[test]
    fn test_read_chunk_one() {
        let mut reader = io::BufReader::new(b"this is\none chunk\n" as &[u8]);
        assert_eq!("this is\none chunk\n", read_chunk(&mut reader).unwrap().unwrap());
        assert!(read_chunk(&mut reader).unwrap().is_none());

        let mut reader = io::BufReader::new(b"this is\none chunk without newline" as &[u8]);
        assert_eq!("this is\none chunk without newline", read_chunk(&mut reader).unwrap().unwrap());
        assert!(read_chunk(&mut reader).unwrap().is_none());
    }

    #[test]
    fn test_read_chunk_many() {
        let mut reader = io::BufReader::new(b"first\nchunk\n\nsecond chunk\n\nthird\n" as &[u8]);
        assert_eq!("first\nchunk\n", read_chunk(&mut reader).unwrap().unwrap());
        assert_eq!("second chunk\n", read_chunk(&mut reader).unwrap().unwrap());
        assert_eq!("third\n", read_chunk(&mut reader).unwrap().unwrap());
        assert!(read_chunk(&mut reader).unwrap().is_none());
    }

    #[test]
    fn test_read_chunk_empty_in_between_is_not_eof() {
        let mut reader = io::BufReader::new(b"first\n\n\n\nsecond" as &[u8]);
        assert_eq!("first\n", read_chunk(&mut reader).unwrap().unwrap());
        assert_eq!("\n", read_chunk(&mut reader).unwrap().unwrap());
        assert_eq!("second", read_chunk(&mut reader).unwrap().unwrap());
        assert!(read_chunk(&mut reader).unwrap().is_none());
    }

    #[test]
    fn test_read_chunk_empty_spurious_newlines() {
        let mut reader = io::BufReader::new(b"first\n\n\nsecond" as &[u8]);
        assert_eq!("first\n", read_chunk(&mut reader).unwrap().unwrap());
        assert_eq!("\nsecond", read_chunk(&mut reader).unwrap().unwrap());
        assert!(read_chunk(&mut reader).unwrap().is_none());
    }

    /// Syntactic sugar to instantiate a new `Step::Map` for testing purposes only.
    fn new_map_step<P: AsRef<Path>>(path: P, underlying_path: P, writable: bool) -> Step {
        Step::Map(Mapping {
            path: PathBuf::from(path.as_ref()),
            underlying_path: PathBuf::from(underlying_path.as_ref()),
            writable: writable,
        })
    }

    /// Syntactic sugar to instantiate a new `Step::Unmap` for testing purposes only.
    fn new_unmap_step<P: AsRef<Path>>(path: P) -> Step {
        Step::Unmap(PathBuf::from(path.as_ref()))
    }

    #[test]
    fn test_parse_chunk_ok() {
        let chunk = r#"[
            {"Map": {"Mapping": "/foo/./bar", "Target": "/bin", "Writable": false}},
            {"Unmap": "/baz"},
            {"Map": {"Mapping": "/", "Target": "/c/d/e", "Writable": true}}
        ]"#;

        let exp_steps = vec![
            new_map_step("/foo/bar", "/bin", false),
            new_unmap_step("/baz"),
            new_map_step("/", "/c/d/e", true),
        ];
        assert_eq!(exp_steps, parse_chunk(&chunk).unwrap());
    }

    #[test]
    fn test_parse_chunk_syntax_error() {
        let chunk = r#"[{"Unmap": "/"},]"#;

        let error = format!("{}", parse_chunk(&chunk).unwrap_err());
        assert!(error.contains("trailing comma"), "Did not find JSON error in '{}'", error);
    }

    #[test]
    fn test_parse_chunk_bad_json_layout() {
        let chunk = r#"[
            {"Map": {"Mapping": "/foo/bar", "Target": "/bin", "Writable": false}},
            {"UnmapFoo": "/baz"},
        ]"#;

        let error = format!("{}", parse_chunk(&chunk).unwrap_err());
        assert!(error.contains("UnmapFoo"), "Did not find 'UnmapFoo' in '{}'", error);
    }


    #[test]
    fn test_parse_chunk_bad_mapping() {
        let chunk = r#"[
            {"Map": {"Mapping": "foo/bar", "Target": "/bin", "Writable": false}}
        ]"#;

        let error = format!("{}", parse_chunk(&chunk).unwrap_err());
        // Make sure the construction of mappings from the configuration steps goes through the
        // same Mapping::from_parts() function that we use to parse the command-line steps.
        assert!(error.contains("path \"foo/bar\" is not absolute"),
            "Did not find Mapping::from_parts() error in '{}'", error);
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
        fn map(&self, mapping: &Mapping) -> Result<(), Error> {
            self.log.lock().unwrap().push(format!("map {}", mapping.path.display()));
            Ok(())
        }

        fn unmap<P: AsRef<Path>>(&self, path: P) -> Result<(), Error> {
            self.log.lock().unwrap().push(format!("unmap {}", path.as_ref().display()));
            Ok(())
        }
    }

    fn do_run_loop_test(input: &str, exp_output: &str, exp_log: Vec<String>) {
        let fs: MockFS = Default::default();

        let mut output: Vec<u8> = Vec::new();
        {
            let reader = io::BufReader::new(input.as_bytes());
            let writer = io::BufWriter::new(&mut output);
            run_loop(reader, writer, &fs);
        }

        assert_eq!(exp_output, String::from_utf8(output).unwrap());
        assert_eq!(exp_log, fs.get_log());
    }

    #[test]
    fn test_run_loop_empty() {
        let input = r#""#;
        let exp_output = "";
        let exp_log = vec![];
        do_run_loop_test(&input, &exp_output, exp_log);
    }

    #[test]
    fn test_run_loop_one() {
        let input = r#"[
            {"Map": {"Mapping": "/foo/bar", "Target": "/bin", "Writable": false}}
        ]"#;
        let exp_output = "Done\n";
        let exp_log = vec![
            String::from("map /foo/bar"),
        ];
        do_run_loop_test(&input, &exp_output, exp_log);
    }

    #[test]
    fn test_run_loop_many() {
        let input = r#"[
            {"Map": {"Mapping": "/foo/bar", "Target": "/bin", "Writable": false}},
            {"Unmap": "/a"}
        ]

        [
            {"Map": {"Mapping": "/z", "Target": "/b", "Writable": false}}
        ]"#;
        let exp_output = "Done\nDone\n";
        let exp_log = vec![
            String::from("map /foo/bar"),
            String::from("unmap /a"),
            String::from("map /z"),
        ];
        do_run_loop_test(&input, &exp_output, exp_log);
    }

    #[test]
    fn test_run_loop_errors() {
        let input = r#"[
            {"Map": {"Mapping": "/foo", "Target": "/bin", "Writable": false}}
        ]

        [
            {"Map": {"Mapping": "bar", "Target": "/b", "Writable": false}}
        ]

        [
            {"Map": {"Mapping": "/z", "Target": "/b", "Writable": false}}
        ]"#;
        let exp_output = "Done\nReconfig failed: path \"bar\" is not absolute\nDone\n";
        let exp_log = vec![
            String::from("map /foo"),
            String::from("map /z"),
        ];
        do_run_loop_test(&input, &exp_output, exp_log);
    }
}
