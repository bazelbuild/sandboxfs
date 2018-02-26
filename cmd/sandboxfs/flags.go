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
	"github.com/bazelbuild/sandboxfs/internal/sandbox"
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

// mappingFlag holds the value of a collection of flags that specify how to configure
// the mappings within a sandboxfs instance.
type mappingFlag []sandbox.MappingSpec

// String returns the textual value of the flag.
func (f *mappingFlag) String() string {
	return fmt.Sprint(*f)
}

// Set parses the value of the flag as given by the user.
func (f *mappingFlag) Set(cmd string) error {
	fields := strings.SplitN(cmd, ":", 3)
	if len(fields) != 3 {
		return fmt.Errorf("flag %q: expected contents to be of the form TYPE:MAPPING:TARGET", cmd)
	}
	typeName := fields[0]
	mapping := filepath.Clean(fields[1])
	target := filepath.Clean(fields[2])
	if !filepath.IsAbs(mapping) {
		return fmt.Errorf("path %q: mapping must be an absolute path", mapping)
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("path %q: target must be an absolute path", target)
	}
	switch typeName {
	case "ro":
		*f = append(*f, sandbox.MappingSpec{
			Mapping:  mapping,
			Target:   target,
			Writable: false,
		})
	case "rw":
		*f = append(*f, sandbox.MappingSpec{
			Mapping:  mapping,
			Target:   target,
			Writable: true,
		})
	default:
		return fmt.Errorf("flag %q: unknown type %s; must be one of ro,rw", cmd, typeName)
	}
	return nil
}
