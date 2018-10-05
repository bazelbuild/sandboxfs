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

package utils

import (
	"fmt"
	"path/filepath"
)

// Config represents the configuration for the integration tests as provided in the command line.
type Config struct {
	// ReleaseBinary is true if the binary provided in SandboxfsBinary was built in release
	// mode, false otherwise.
	ReleaseBinary bool

	// True if using the new Rust variant of sandboxfs.  Should only be used to change the
	// behavior of tests due to cosmetic details.
	RustVariant bool

	// SandboxfsBinary contains the absolute path to the sandboxfs binary to test.
	SandboxfsBinary string

	// UnprivilegedUser is a non-root user to use when the integration tests are run as root,
	// by those tests that require dropping privileges.  May be nil, in which case those tests
	// are skipped.
	UnprivilegedUser *UnixUser
}

// globalConfig contains the singleton instance of the configuration.  This must be initialized at
// test program startup time with the SetConfigFromFlags function and can later be queried at will
// by any test.
var globalConfig *Config

// SetConfigFromFlags initializes the test configuration based on the raw values provided by the
// user on the command line.  Returns an error if any of those values is incorrect.
func SetConfigFromFlags(releaseBinary bool, rustVariant bool, rawSandboxfsBinary string, unprivilegedUserName string) error {
	if globalConfig != nil {
		panic("SetConfigFromFlags can only be called once")
	}

	sandboxfsBinary, err := filepath.Abs(rawSandboxfsBinary)
	if err != nil {
		return fmt.Errorf("cannot make %s absolute: %v", rawSandboxfsBinary, err)
	}

	var unprivilegedUser *UnixUser
	if unprivilegedUserName != "" {
		unprivilegedUser, err = LookupUser(unprivilegedUserName)
		if err != nil {
			return fmt.Errorf("invalid unprivileged user setting %s: %v", unprivilegedUserName, err)
		}
	}

	globalConfig = &Config{
		ReleaseBinary:    releaseBinary,
		RustVariant:      rustVariant,
		SandboxfsBinary:  sandboxfsBinary,
		UnprivilegedUser: unprivilegedUser,
	}
	return nil
}

// GetConfig returns the singleon instance of the test configuration.
func GetConfig() *Config {
	if globalConfig == nil {
		panic("GetConfig should have been called from main but was not yet")
	}

	return globalConfig
}
