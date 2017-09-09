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

// Package sandbox provides the implementation of sandboxfs, a FUSE based
// filsystem for the purpose of sandboxing.
package sandbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/sys/unix"
)

// lastInodeNumber is a counter that contains the last allotted inode number.
var lastInodeNumber struct {
	counter uint64
	mu      sync.Mutex
}

// MappingSpec stores the specification for a mapping in the filesystem.
type MappingSpec struct {
	Mapping  string
	Target   string
	Writable bool
}

// DynamicConf stores the configuration for a dynamic mount point.
type DynamicConf struct {
	Input  io.Reader
	Output io.Writer
}

// nextInodeNumber returns the next unused inode number.
func nextInodeNumber() uint64 {
	lastInodeNumber.mu.Lock()
	defer lastInodeNumber.mu.Unlock()
	lastInodeNumber.counter++
	return lastInodeNumber.counter
}

// FS is the global unique representation of the filesystem tree.
type FS struct {
	root *Root
}

// Reconfigure resets the tree under this node to the new configuration.
func (f *FS) Reconfigure(server *fs.Server, root dirCommon) {
	// TODO(pallavag): Right now, we do not reuse inode numbers, because it is
	// uncertain if doing so would be valid from a correctness perspective.
	// Once the code has been well tested, it may be worthwile to try resetting
	// the inode counter at this location.
	// Also, it may be desirable to keep a track of all open file handles, and
	// close them before reconfiguration. Alternatively, we may want to close
	// only the handles that no longer appear in the new configuration, or are
	// relocated. Or just disallow reconfiguration when there are open file
	// handles. Needs more thought; think about what happens when one tries to
	// unmount a file system with open handles.
	f.root.Reconfigure(server, root)
}

// readConfig reads one chunk of config from reader.
// An empty line indicates the end of one config.
func readConfig(reader *bufio.Reader) ([]byte, error) {
	configRead := make([]byte, 0, 2048)
	for {
		input, err := reader.ReadBytes('\n')
		configRead = append(configRead, input...)
		if err != nil && err != io.EOF {
			return configRead, err
		}
		if len(input) == 1 && input[0] == '\n' {
			break // empty line = end of config.
		}
	}
	return configRead, nil
}

// initFromReader initializes a filesystem configuration after reading the
// config from the passed reader.
func initFromReader(reader *bufio.Reader) (dirCommon, error) {
	configRead, err := readConfig(reader)
	if err != nil {
		return nil, fmt.Errorf("unable to read config: %v", err)
	}
	configArgs := make([]MappingSpec, 0)
	if err := json.Unmarshal(configRead, &configArgs); err != nil {
		return nil, fmt.Errorf("unable to parse json: %v", err)
	}
	sfs, err := Init(configArgs)
	if err != nil {
		return nil, fmt.Errorf("sandbox init failed with: %v", err)
	}
	return sfs, nil
}

// reconfigurationListener monitors the input for a configuration, and
// reconfigures the filesystem once a valid configuration is read.
func reconfigurationListener(server *fs.Server, filesystem *FS, input io.Reader, output io.Writer) {
	reader := bufio.NewReader(input)
	for {
		sfs, err := initFromReader(reader)
		if err != nil {
			fmt.Fprintf(output, "Reconfig failed: %v\n", err)
			continue
		}
		filesystem.Reconfigure(server, sfs)
		fmt.Fprintln(output, "Done")
	}
}

// Serve sets up the work environment before starting to serve the filesystem.
func Serve(c *fuse.Conn, node dirCommon, dynamic *DynamicConf) error {
	f := &FS{NewRoot(node)}
	server := fs.New(c, nil)
	if dynamic != nil {
		if !c.Protocol().HasInvalidate() {
			return fmt.Errorf("kernel does not support cache invalidation; required by dynamic sandbox")
		}
		go reconfigurationListener(server, f, dynamic.Input, dynamic.Output)
	}
	oldMask := unix.Umask(0)
	defer unix.Umask(oldMask)
	return server.Serve(f)
}

// Init initializes a new fs.Node instance to represent a sandboxfs file system
// rooted in the given directory.
func Init(mappingsInput []MappingSpec) (dirCommon, error) {
	// Clone the list so that we don't modify the passed slice.
	mappings := append([]MappingSpec(nil), mappingsInput...)

	// This sort helps us ensure that a mapping will only have to be created
	// where one either does not exist or is a VirtualDir (since the more
	// nested mappings will be handled first).
	// Note that the order of different paths with the same length doesn't
	// matter since one can never be a prefix of the other.
	sort.Slice(mappings, func(i, j int) bool {
		return len(mappings[i].Mapping) > len(mappings[j].Mapping)
	})

	root := newVirtualDir()
	for _, mapping := range mappings {
		curDir := root
		components := splitPath(mapping.Mapping)
		for i, component := range components {
			if i != len(components)-1 {
				curDir = curDir.virtualDirChild(component)
			} else {
				if _, err := curDir.newNodeChild(component, mapping.Target, mapping.Writable); err != nil {
					return nil, fmt.Errorf("mapping %v: %v", mapping.Mapping, err)
				}
			}
		}
	}

	node, err := root.lookup("")
	if err != nil {
		// If we reach here, it means sandbox config was empty.
		return root, nil
	}
	if n, ok := node.(dirCommon); ok {
		return n, nil
	}
	return nil, fmt.Errorf("can't map a file at root; must be a directory")
}

// Root returns the root node from the filesystem object.
func (f *FS) Root() (fs.Node, error) {
	return f.root, nil
}

// SetRoot sets the root node for the filesystem object.
func (f *FS) SetRoot(root *Root) {
	f.root = root
}

// timespecToTime converts unix.Timespec to time.Time.
func timespecToTime(ts syscall.Timespec) time.Time {
	return time.Unix(int64(ts.Sec), int64(ts.Nsec))
}

// splitPath converts a path string to a list of components of the path.
//
// Absolute and relative paths are treated the same way. The empty string as
// the first element enables us to handle all paths in the same way, without
// having to add zero-length checks for the returned list.
// For example: "a/b" -> ["", "a", "b"] and "/a/b" -> ["", "a", "b"].
//
// Root, '.', and the empty string all return [""].
func splitPath(path string) []string {
	path = filepath.Clean(path)
	if path == "/" || path == "." {
		return []string{""}
	}
	return append([]string{""}, strings.Split(strings.TrimPrefix(path, "/"), "/")...)
}

// fuseErrno converts different types of errors returned by different packages
// into fuse.Errno which is accepted by the fuse package.
func fuseErrno(e error) error {
	switch err := e.(type) {
	case fuse.Errno:
		return err
	case *os.PathError:
		return fuseErrno(err.Err)
	case syscall.Errno:
		return fuse.Errno(err)
	case *os.LinkError:
		return fuseErrno(err.Err)
	case *os.SyscallError:
		return fuseErrno(err.Err)
	}
	switch e {
	case os.ErrInvalid:
		return fuse.Errno(unix.EINVAL)
	case os.ErrPermission:
		return fuse.Errno(unix.EPERM)
	case os.ErrExist:
		return fuse.Errno(unix.EEXIST)
	case os.ErrNotExist:
		return fuse.Errno(unix.ENOENT)
	case os.ErrClosed:
		return fuse.Errno(unix.EBADF)
	}
	return e
}

func logCacheInvalidationError(e error, info ...interface{}) {
	switch e {
	case nil, fuse.ErrNotCached: // ignored errors
		return
	}
	info = append(info, ": ", e)
	log.Print(info...)
}
