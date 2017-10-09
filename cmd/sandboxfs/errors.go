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

package main

import "fmt"

// mountError represents an error caused by the mount operation.
type mountError struct {
	message string
}

// Error returns the formatted usage error message.
func (e *mountError) Error() string {
	return e.message
}

// newMountError constructs a new mountError given a format string and positional arguments.
func newMountError(format string, arg ...interface{}) error {
	return &usageError{
		message: fmt.Sprintf(format, arg...),
	}
}

// usageError represents an error caused by the user invoking the command with an invalid syntax
// (bad number of arguments, bad flag names, bad flag values, etc.).
type usageError struct {
	message string
}

// Error returns the formatted usage error message.
func (e *usageError) Error() string {
	return e.message
}

// newUsageError constructs a new usageError given a format string and positional arguments.
func newUsageError(format string, arg ...interface{}) error {
	return &usageError{
		message: fmt.Sprintf(format, arg...),
	}
}
