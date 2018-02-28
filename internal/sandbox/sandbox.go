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

// Step represents a single reconfiguration operation.
type Step struct {
	// Map requests that a new mapping be added to the file system.
	Map *MappingSpec

	// Unmap requests that the given mapping is removed from the file system.
	Unmap string
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
func initFromReader(reader *bufio.Reader) ([]Step, error) {
	configRead, err := readConfig(reader)
	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("unable to read config: %v", err)
	}
	steps := []Step{}
	if err := json.Unmarshal(configRead, &steps); err != nil {
		return nil, fmt.Errorf("unable to parse json: %v", err)
	}
	return steps, nil
}

// reconfigurationListener monitors the input for a configuration, and
// reconfigures the filesystem once a valid configuration is read.
func reconfigurationListener(server *fs.Server, filesystem *FS, input io.Reader, output io.Writer) {
	reader := bufio.NewReader(input)
	for {
		config, err := initFromReader(reader)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Fprintf(output, "Reconfig failed: %v\n", err)
			continue
		}
		if err := filesystem.Reconfigure(server, config); err != nil {
			fmt.Fprintf(output, "Reconfig failed: %v\n", err)
			continue
		}
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

// tokenizeMapping builds upon tokenizePath to split a path into its components and validate that
// the result is a valid mapping.
func tokenizeMapping(path string) ([]string, error) {
	components := tokenizePath(path)
	if len(components) == 0 {
		return nil, fmt.Errorf("invalid mapping %s: empty path", path)
	} else if components[0] != "" {
		return nil, fmt.Errorf("invalid mapping %s: must be an absolute path", path)
	}
	return components, nil
}

// applyMapping adds a new mapping to the given root directory.  If the root directory is nil and
// the mapping is valid, this returns a newly-instantiated root directory; otherwise, the returned
// root directory matches the input root directory.
func applyMapping(root *Dir, mapping *MappingSpec) (*Dir, error) {
	components, err := tokenizeMapping(mapping.Mapping)
	if err != nil {
		return nil, err
	}
	if len(components) == 1 && root != nil {
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
	return root, nil
}

// reconfigureMap applies a single Map step to the file system.
func (f *FS) reconfigureMap(mapping *MappingSpec) error {
	// TODO(jmmv): We cheat and peek directly into root to get its directory.  The directory
	// could change with previous implementations of reconfiguration but cannot any longer.
	// Remove once the Root node indirection is gone.
	root := f.root.getDir()
	newRoot, err := applyMapping(root, mapping)
	if err != nil {
		return err
	}
	if root != newRoot {
		// applyMapping above has the ability to generate a new root directory when none is
		// given to it.  We should never encounter that situation here because we do not
		// allow mapping reconfigurations to modify the root, so ensure this is true.
		panic("Mapping should not have changed the root but did")
	}
	return nil
}

// reconfigureUnmap applies a single Unmap step to the file system.
//
// Note that this only removes the leaf of the given mapping: nested entries created during a map
// operation are left behind and have to be unmapped explicitly.  E.g. if /foo/bar/baz was mapped
// on an empty file system, /foo/bar are two intermediate mappings that are not removed when
// /foo/var/baz is unmapped.  This is for simplicity as we'd otherwise need to track whether a
// mapping was explicitly created by the user or as a result of another mapping, and also to keep
// the unmapping algorithm trivial.
func (f *FS) reconfigureUnmap(server *fs.Server, mapping string) error {
	components, err := tokenizeMapping(mapping)
	if err != nil {
		return err
	}
	if len(components) == 1 {
		return fmt.Errorf("cannot unmap root")
	}
	node := f.root.dir.LookupOrFail(components[1 : len(components)-1])
	if node == nil {
		return fmt.Errorf("failed to unmap %s: intermediate components not mapped", mapping)
	}
	// TODO(jmmv): Determining the "identity" of the node exists purely to deal with the fact
	// that Root is a container for a directory that could mutate with previous implementations
	// of reconfiguration.  Remove the Root indirection and this hack.
	var identity fs.Node
	identity = f.root
	if len(components) > 2 {
		identity = node
	}
	if err := node.Unmap(server, identity, components[len(components)-1]); err != nil {
		return fmt.Errorf("failed to unmap %s: %v", mapping, err)
	}
	return nil
}

// Reconfigure applies a reconfiguration request to the file system.
func (f *FS) Reconfigure(server *fs.Server, steps []Step) error {
	// TODO(jmmv): This essentially implements an RPC system with two functions.  Investigate
	// whether we can replace this with local gRPC (re. performance).
	for _, step := range steps {
		if step.Map != nil && step.Unmap != "" {
			return fmt.Errorf("invalid step: Map and Unmap are exclusive")
		} else if step.Map != nil {
			if err := f.reconfigureMap(step.Map); err != nil {
				return err
			}
		} else if step.Unmap != "" {
			if err := f.reconfigureUnmap(server, step.Unmap); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("invalid step: neither Map nor Unmap were defined")
		}
	}
	return nil
}

// CreateRoot generates a directory tree to represent the given mappings.
func CreateRoot(root *Dir, mappings []MappingSpec) (*Dir, error) {
	for _, mapping := range mappings {
		var err error
		root, err = applyMapping(root, &mapping)
		if err != nil {
			return nil, err
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
