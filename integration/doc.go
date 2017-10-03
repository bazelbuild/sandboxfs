// Copyright 2017 Google Inc.
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

// Package integration contains integration tests for sandboxfs.
//
// All the tests in this package are designed to execute a sandboxfs binary and treat it as a black
// box.  The binary to be tested is indicated by the SANDBOXFS environment variable, which must be
// set by the user at startup time.
package integration
