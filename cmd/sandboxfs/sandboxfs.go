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

// The sandboxfs binary mounts an instance of the sandboxfs filesystem.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"bazil.org/fuse"
	"flag"
	"log"

	"github.com/bazelbuild/sandboxfs/internal/sandbox"
)

// MappingTargetPair stores a single mapping of the form mapping->target.
type MappingTargetPair struct {
	Mapping string
	Target  string
}

// combineToSpec combines the entries from roMappings and rwMappings into
// a single collection that is sufficient to provide the mapping specification.
func combineToSpec(roMappings, rwMappings []MappingTargetPair) []sandbox.MappingSpec {
	mapped := make([]sandbox.MappingSpec, 0, len(roMappings)+len(rwMappings))
	for _, mapping := range roMappings {
		mapped = append(mapped, sandbox.MappingSpec{
			Mapping:  mapping.Mapping,
			Target:   mapping.Target,
			Writable: false,
		})
	}
	for _, mapping := range rwMappings {
		mapped = append(mapped, sandbox.MappingSpec{
			Mapping:  mapping.Mapping,
			Target:   mapping.Target,
			Writable: true,
		})
	}
	return mapped
}

// unmountOnSignal captures termination signals and unmounts the filesystem.
// This allows the main program to exit the FUSE serving loop cleanly and avoids
// leaking a mount point without the backing FUSE server.
func unmountOnSignal(mountPoint string, caught chan<- os.Signal) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	caught <- <-c // Wait for signal.
	signal.Reset()
	err := fuse.Unmount(mountPoint)
	if err != nil {
		log.Printf("Unmounting filesystem failed with error: %v", err)
	}
}

func usage(output io.Writer, f *flag.FlagSet) {
	f.SetOutput(output)
	f.PrintDefaults()
}

type mappingFlag []MappingTargetPair

func (f *mappingFlag) String() string {
	return fmt.Sprint(*f)
}

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

func (f *mappingFlag) Get() interface{} {
	return *f
}

func staticCommand(static *flag.FlagSet) error {
	roMappings := static.Lookup("read_only_mapping").Value.(*mappingFlag)
	rwMappings := static.Lookup("read_write_mapping").Value.(*mappingFlag)
	if v := static.Lookup("help").Value; v.String() == "true" {
		fmt.Fprintf(os.Stdout, "Usage: %s static [flags...] MOUNT-POINT\n", filepath.Base(os.Args[0]))
		usage(os.Stdout, static)
		return nil
	}
	if static.NArg() != 1 {
		return fmt.Errorf("Invalid number of arguments; pass -help flag for details")
	}
	return serve(static.Arg(0), nil, combineToSpec(*roMappings, *rwMappings))
}

func dynamicCommand(dynamic *flag.FlagSet) error {
	dynamicConf := &sandbox.DynamicConf{Input: os.Stdin, Output: os.Stdout}
	if v := dynamic.Lookup("help").Value; v.String() == "true" {
		fmt.Fprintf(os.Stdout, "Usage: %s dynamic MOUNT-POINT\n", filepath.Base(os.Args[0]))
		usage(os.Stdout, dynamic)
		return nil
	}
	if dynamic.NArg() != 1 {
		return fmt.Errorf("Invalid number of arguments; pass -help flag for details")
	}
	if v := dynamic.Lookup("input").Value; v.String() != "-" {
		file, err := os.Open(v.String())
		if err != nil {
			return fmt.Errorf("Unable to open file %q for reading: %v", v.String(), err)
		}
		defer file.Close()
		dynamicConf.Input = file
	}
	if v := dynamic.Lookup("output").Value; v.String() != "-" {
		file, err := os.Create(v.String())
		if err != nil {
			return fmt.Errorf("Unable to open file %q for writing: %v", v.String(), err)
		}
		defer file.Close()
		dynamicConf.Output = file
	}
	return serve(dynamic.Arg(0), dynamicConf, nil)
}

func serve(mountPoint string, dynamicConf *sandbox.DynamicConf, mappings []sandbox.MappingSpec) error {
	sfs, err := sandbox.Init(mappings)
	if err != nil {
		return fmt.Errorf("Unable to init sandbox: %v", err)
	}

	// OSXFUSE unconditionally creates the mount point if it does not exist while Linux's FUSE
	// errors out on this condition. Linux is behaving correctly here, but to unify the behavior
	// between the two cases (and, especially, to ensure that the error message that we print is
	// consistent), explicitly test for the mount point's existence.
	//
	// Note that this is knowingly racy. If the mount point is created after this call but before
	// the actual mount operation happens, we'll be subject to OS-specific behavior. We cannot do
	// do better, and this is not a big deal anyway.
	//
	// TODO(jmmv): Fix OSXFUSE to not create the mount point. If that's undesirable upstream, an
	// alternative would be to add a "-o nocreate_mount" option to mount_osxfuse and then use that
	// in the fuse.Mount call below.
	if _, err := os.Lstat(mountPoint); os.IsNotExist(err) {
		return fmt.Errorf("Unable to mount: %s does not exist", mountPoint)
	}

	caughtSignal := make(chan os.Signal, 1)
	go unmountOnSignal(mountPoint, caughtSignal)
	c, err := fuse.Mount(
		mountPoint,
		fuse.FSName("sandboxfs"),
		fuse.Subtype("sandboxfs"),
		fuse.LocalVolume(),
		fuse.VolumeName(flag.Lookup("volume_name").Value.String()),
	)
	if err != nil {
		return fmt.Errorf("Unable to mount: %v", err)
	}
	defer c.Close()

	err = sandbox.Serve(c, sfs, dynamicConf)
	if err != nil {
		return fmt.Errorf("FileSystem server error: %v", err)
	}

	<-c.Ready
	if err := c.MountError; err != nil {
		return fmt.Errorf("Mount error: %v", err)
	}

	// If we reach this point, the FUSE serve loop has terminated either
	// because the user unmounted the file system, in which case we have to
	// exit cleanly; or we have received a signal, in which case we have to
	// redeliver it to ourselves to terminate with the correct exit status.
	select {
	case signal := <-caughtSignal:
		// Note that the signal handler unblocked the Serve call by unmounting
		// the file system, which means that it's now safe to kill the daemon
		// without leaking the mount point.
		// TODO(pallavag): This causes the defers (closing files for instance)
		// to never execute. It may be okay to remove this signal re-delivering
		// and just exit cleanly on caught signals.
		p, err := os.FindProcess(os.Getpid())
		if err != nil {
			return fmt.Errorf("Unable to retrieve process: %v", err)
		}
		p.Signal(signal)
		for {
			time.Sleep(1 * time.Second) // Wait for signal to be received.
		}
	default:
	}
	return nil
}

func main() {
	var readOnlyMappings, readWriteMappings mappingFlag
	flag.Usage = func() {} // Suppress default output.
	debug := flag.Bool("debug", false, "log details about FUSE requests and responses to stderr")
	help := flag.Bool("help", false, "print the usage information and exit")
	flag.String("volume_name", "sandbox", "name for the sandboxfs volume")

	static := flag.NewFlagSet("static", flag.ExitOnError)
	static.Usage = func() {}
	static.Var(&readOnlyMappings, "read_only_mapping", "read-only mapping of the form MAPPING:TARGET")
	static.Var(&readWriteMappings, "read_write_mapping", "read/write mapping of the form MAPPING:TARGET")
	static.Bool("help", false, "print the usage information and exit")

	dynamic := flag.NewFlagSet("dynamic", flag.ExitOnError)
	dynamic.Bool("help", false, "print the usage information and exit")
	dynamic.String("input", "-", "where to read the configuration data from (- for stdin)")
	dynamic.String("output", "-", "where to write the status of reconfiguration to (- for stdout)")
	dynamic.Usage = func() {}

	flag.Parse()

	if *help {
		fmt.Fprintf(os.Stdout, `Usage: %s [flags...] subcommand ...
Subcommands:
  static   statically configured sandbox using command line flags.
  dynamic  dynamically configured sandbox using stdin.
Flags:
`, filepath.Base(os.Args[0]))
		usage(os.Stdout, flag.CommandLine)
		os.Exit(0)
	}

	if *debug {
		fuse.Debug = func(msg interface{}) { fmt.Fprintln(os.Stderr, msg) }
	}

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Invalid number of arguments; pass -help flag for details\n")
		os.Exit(1)
	}

	var err error
	switch flag.Arg(0) {
	case "static":
		static.Parse(flag.Args()[1:])
		err = staticCommand(static)
	case "dynamic":
		dynamic.Parse(flag.Args()[1:])
		err = dynamicCommand(dynamic)
	default:
		fmt.Fprintf(os.Stderr, "Invalid command; pass -help flag for details\n")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
