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

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // Automatically exports pprof endpoints.
	"os"
	"runtime"
	"runtime/pprof"
)

// ProfileSettings describes the profiling features to be enabled at serve time.
type ProfileSettings struct {
	// CPUProfile contains the path to which to write a CPU profile on exit, if not empty.
	CPUProfile string

	// ListenAddress contains the host:port address of the HTTP server to start, if not empty.
	ListenAddress string

	// MemProfile contains the path to which to write a memory profile on exit, if not empty.
	MemProfile string
}

// NewProfileSettings creates an instance of profileSettings from flag values, verifying that their
// combination is valid.
func NewProfileSettings(cpuProfile string, memProfile string, listenAddress string) (ProfileSettings, error) {
	if (cpuProfile != "" || memProfile != "") && listenAddress != "" {
		return ProfileSettings{}, fmt.Errorf("file-based CPU or memory profiling are incompatible with a listening address")
	}

	return ProfileSettings{
		CPUProfile:    cpuProfile,
		ListenAddress: listenAddress,
		MemProfile:    memProfile,
	}, nil
}

// profileContext holds runtime status about the enabled profiling features.
type profileContext struct {
	cpuFile io.WriteCloser
	memFile io.WriteCloser
}

// StartProfiling enables the profiling features requested by profileSettings.  The caller is
// responsible for closing the opaque returned context object once the code section to be profiled
// finishes.
func StartProfiling(s ProfileSettings) (io.Closer, error) {
	success := false

	var cpuFile io.WriteCloser
	if s.CPUProfile != "" {
		var err error
		cpuFile, err = os.Create(s.CPUProfile)
		if err != nil {
			return nil, fmt.Errorf("failed to create CPU profile %s: %v", s.CPUProfile, err)
		}
		defer func() {
			if !success {
				cpuFile.Close()
			}
		}()

		pprof.StartCPUProfile(cpuFile)
	}

	var memFile io.WriteCloser
	if s.MemProfile != "" {
		var err error
		memFile, err = os.Create(s.MemProfile)
		if err != nil {
			return nil, fmt.Errorf("failed to create memory profile %s: %v", s.MemProfile, err)
		}
		defer func() {
			if !success {
				memFile.Close()
			}
		}()
	}

	if s.ListenAddress != "" {
		l, err := net.Listen("tcp", s.ListenAddress)
		if err != nil {
			return nil, fmt.Errorf("failed to start HTTP server: %v", err)
		}
		log.Printf("starting HTTP server on %v", l.Addr())

		go func() {
			err := http.Serve(l, nil)
			log.Printf("failed to start HTTP server: %v", err)
		}()
	}

	// All operations that can fail are now done.  Setting success=true prevents any deferred
	// cleanup routines from running, so any code below this line must not be able to fail.
	success = true
	return &profileContext{
		cpuFile: cpuFile,
		memFile: memFile,
	}, nil
}

// Close stops profiling and dumps any requested logs to disk.
func (c *profileContext) Close() error {
	if c.cpuFile != nil {
		pprof.StopCPUProfile()
		c.cpuFile.Close()
		c.cpuFile = nil
	}

	if c.memFile != nil {
		runtime.GC() // Get up-to-date statistics.
		if err := pprof.WriteHeapProfile(c.memFile); err != nil {
			log.Printf("could not write memory profile: %v", err)
		}
		c.memFile.Close()
		c.memFile = nil
	}

	return nil
}
