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

package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"bazil.org/fuse"
)

// allowFlag holds the value of and parses a flag that controls who has access to the file system.
type allowFlag struct {
	// Option is the FUSE mount option to pass to the mount operation, or nil if not applicable.
	Option fuse.MountOption

	// value is the textual representation of the flag's value.
	value string
}

// String returns the textual value of the flag.
func (f *allowFlag) String() string {
	return f.value
}

// Set parses the value of the flag as given by the user.
func (f *allowFlag) Set(value string) error {
	switch value {
	case "other":
		f.Option = fuse.AllowOther()
	case "root":
		f.Option = fuse.AllowRoot()
	case "self":
		f.Option = nil
	default:
		return fmt.Errorf("must be one of other, root, or self")
	}
	f.value = value
	return nil
}

// MappingTargetPair stores a single mapping of the form mapping->target.
type MappingTargetPair struct {
	Mapping string
	Target  string
}

// mappingFlag holds the value of a collection of flags that specify how to configure
// the mappings within a sandboxfs instance.
type mappingFlag []MappingTargetPair

// String returns the textual value of the flag.
func (f *mappingFlag) String() string {
	return fmt.Sprint(*f)
}

// Set parses the value of the flag as given by the user.
func (f *mappingFlag) Set(cmd string) error {
	fields := strings.SplitN(cmd, ":", 2)
	if len(fields) != 2 {
		return fmt.Errorf("flag %q: expected contents to be of the form MAPPING:TARGET", cmd)
	}
	mapping := filepath.Clean(fields[0])
	target := filepath.Clean(fields[1])
	if !filepath.IsAbs(mapping) {
		return fmt.Errorf("path %q: mapping must be an absolute path", fields[0])
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("path %q: target must be an absolute path", fields[1])
	}
	*f = append(*f, MappingTargetPair{
		Mapping: mapping,
		Target:  target,
	})
	return nil
}
