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
func (f *FS) Reconfigure(server *fs.Server, root *Dir) {
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
		if err != nil {
			return nil, err
		}
		configRead = append(configRead, input...)
		if len(input) == 1 && input[0] == '\n' {
			break // empty line = end of config.
		}
	}
	return configRead, nil
}

// initFromReader initializes a filesystem configuration after reading the config from the passed
// reader.  Reaching EOF on the reader causes this function to return io.EOF, which the caller
// must handle gracefully.
func initFromReader(reader *bufio.Reader) (*Dir, error) {
	configRead, err := readConfig(reader)
	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("unable to read config: %v", err)
	}
	configArgs := make([]MappingSpec, 0)
	if err := json.Unmarshal(configRead, &configArgs); err != nil {
		return nil, fmt.Errorf("unable to parse json: %v", err)
	}
	sfs, err := CreateRoot(configArgs)
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
			if err == io.EOF {
				break
			}
			fmt.Fprintf(output, "Reconfig failed: %v\n", err)
			continue
		}
		filesystem.Reconfigure(server, sfs)
		fmt.Fprintln(output, "Done")
	}
	log.Printf("reached end of input during reconfiguration; file system will continue running until unmounted")
}

// Serve sets up the work environment before starting to serve the filesystem.
func Serve(c *fuse.Conn, dir *Dir, input io.Reader, output io.Writer) error {
	f := &FS{NewRoot(dir)}
	server := fs.New(c, nil)

	if !c.Protocol().HasInvalidate() {
		// We unconditionally require cache invalidation support to implement
		// reconfigurations. If this ever turns to be a problem, we could make them
		// conditional on a flag.
		return fmt.Errorf("kernel does not support cache invalidation")
	}
	go reconfigurationListener(server, f, input, output)

	oldMask := unix.Umask(0)
	defer unix.Umask(oldMask)
	return server.Serve(f)
}

// tokenizePath splits a path into its components. An empty path results in an empty list, and an
// absolute path (including root) results in an empty component as the first item in the result.
func tokenizePath(path string) []string {
	path = filepath.Clean(path)
	if path == "." {
		return []string{}
	} else if path == "/" {
		return []string{""}
	}

	tokens := []string{}
	startIndex := 0
	for i := 0; i < len(path); i++ {
		if os.IsPathSeparator(path[i]) {
			tokens = append(tokens, path[startIndex:i])
			startIndex = i + 1
		}
	}
	tokens = append(tokens, path[startIndex:])
	return tokens
}

// CreateRoot generates a directory tree to represent the given mappings.
func CreateRoot(mappings []MappingSpec) (*Dir, error) {
	var root *Dir
	for _, mapping := range mappings {
		components := tokenizePath(mapping.Mapping)
		if len(components) == 0 {
			return nil, fmt.Errorf("invalid mapping %s: empty path", mapping.Mapping)
		} else if components[0] != "" {
			return nil, fmt.Errorf("invalid mapping %s: must be an absolute path", mapping.Mapping)
		} else if len(components) == 1 && root != nil {
			return nil, fmt.Errorf("failed to map root with target %s: root must be mapped first and not more than once", mapping.Target)
		}

		fileInfo, err := os.Lstat(mapping.Target)
		if err != nil {
			return nil, fmt.Errorf("failed to stat %s when mapping %s: %v", mapping.Target, mapping.Mapping, err)
		}

		if len(components) == 1 {
			if fileInfo.Mode()&os.ModeType != os.ModeDir {
				return nil, fmt.Errorf("cannot map file %s at root: must be a directory", mapping.Target)
			}
			root = newDir(mapping.Target, fileInfo, mapping.Writable)
			root.isMapping = true
		} else {
			if root == nil {
				root = newDirEmpty()
			}
			dirNode := root.LookupOrCreateDirs(components[1 : len(components)-1])
			newNode := getOrCreateNode(mapping.Target, fileInfo, mapping.Writable)
			newNode.SetIsMapping()
			if err := dirNode.Map(components[len(components)-1], newNode); err != nil {
				return nil, fmt.Errorf("cannot map %s: %v", mapping.Mapping, err)
			}
		}
	}
	if root == nil {
		root = newDirEmpty()
	}
	return root, nil
}

// Root returns the root node from the filesystem object.
func (f *FS) Root() (fs.Node, error) {
	return f.root, nil
}

// SetRoot sets the root node for the filesystem object.
func (f *FS) SetRoot(root *Root) {
	f.root = root
}

// timespecToTime converts syscall.Timespec to time.Time.
func timespecToTime(ts syscall.Timespec) time.Time {
	return time.Unix(int64(ts.Sec), int64(ts.Nsec))
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
		return fuse.Errno(syscall.EINVAL)
	case os.ErrPermission:
		return fuse.Errno(syscall.EPERM)
	case os.ErrExist:
		return fuse.Errno(syscall.EEXIST)
	case os.ErrNotExist:
		return fuse.Errno(syscall.ENOENT)
	case os.ErrClosed:
		return fuse.Errno(syscall.EBADF)
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

// UnixMode translates a Go os.FileMode to a Unix mode.
func UnixMode(fileMode os.FileMode) uint32 {
	perm := uint32(fileMode & os.ModePerm)

	var unixType uint32
	switch fileMode & os.ModeType {
	case 0:
		unixType = unix.S_IFREG
	case os.ModeDir:
		unixType = unix.S_IFDIR
	case os.ModeSymlink:
		unixType = unix.S_IFLNK
	case os.ModeNamedPipe:
		unixType = unix.S_IFIFO
	case os.ModeSocket:
		unixType = unix.S_IFSOCK
	case os.ModeDevice:
		if fileMode&os.ModeCharDevice != 0 {
			unixType = unix.S_IFCHR
		} else {
			unixType = unix.S_IFBLK
		}
	default:
		panic(fmt.Sprintf("handling of file mode %v not implemented", fileMode))
	}

	return perm | unixType
}
