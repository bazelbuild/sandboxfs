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
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"bazil.org/fuse"
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

// handleSignals installs signal handlers to ensure the file system is unmounted.
//
// The signal handler is responsible for unmounting the file system, which in turn causes the
// FUSE serve loop to either never start or to finish execution if it was already running.
//
// But this is tricky business because of a potential race: if the signal handler is installed
// and a signal arrives *before* the mount point has been configured, the unmounting will not
// succeed, which means we will enter the server loop and lose the signal. Conversely, if we
// did this backwards, we could receive a signal after the mount point has been configured but
// before we install the signal handler, which means we'd terminate but leak the mount point.
//
// To solve this, we must install the signal handler first, but we must not take action on
// signals until after we know that the mount point has been set up. This is what the mountPoint
// channel is for: the caller must inject the mount point into this handler only after the mount
// operation has succeeded, or an empty string if the mount operation failed.
//
// The caller can know if a signal was the cause of the FUSE serve loop termination by inspecting
// the contents of the caughtSignal output channel.
//
// NOTE: This would be much easier if we could just mask signals while we prepare the mount point in
// the caller and then process them once we are ready. Unfortunately, signal.Ignore() does not only
// mask signals: it discards them as well! The x/sys/unix package is supposed to gain signal
// handling at some point so maybe we'll be able to revisit this in the future? Who knows.
func handleSignals(mountPoint <-chan string, caughtSignal chan<- os.Signal) {
	handler := make(chan os.Signal, 1)
	signal.Notify(handler, syscall.SIGHUP, os.Interrupt, syscall.SIGTERM)

	go func() {
		caughtSignal <- <-handler // Wait for the signal.

		// Wait until the main process has had a chance to issue the mount(2) system call.
		// It doesn't matter if such call succeeded or not: we must wait anyway before
		// attempting to do any cleanup.
		mnt := <-mountPoint

		if mnt != "" {
			// Now the real deal: the main process did actually get to issue a
			// successful mount(2) system call. Make the mount point vanish so that the
			// FUSE serve loop terminates or prevent it from starting.
			err := fuse.Unmount(mnt)
			if err != nil {
				log.Printf("unmounting filesystem failed with error: %v", err)
			}
		}
	}()
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

// staticCommand implements the "static" command.
func staticCommand(args []string, volumeName string, settings ProfileSettings) error {
	flags := flag.NewFlagSet("static", flag.ExitOnError)
	help := flags.Bool("help", false, "print the usage information and exit")
	var readOnlyMappings mappingFlag
	flags.Var(&readOnlyMappings, "read_only_mapping", "read-only mapping of the form MAPPING:TARGET")
	var readWriteMappings mappingFlag
	flags.Var(&readWriteMappings, "read_write_mapping", "read/write mapping of the form MAPPING:TARGET")

	flags.Usage = func() {} // Suppress default output.
	flags.Parse(args)

	if *help {
		fmt.Fprintf(os.Stdout, "Usage: %s static [flags...] MOUNT-POINT\n", filepath.Base(os.Args[0]))
		usage(os.Stdout, flags)
		return nil
	}

	if flags.NArg() != 1 {
		return fmt.Errorf("Invalid number of arguments; pass -help flag for details")
	}
	mountPoint := flags.Arg(0)

	return serve(settings, mountPoint, volumeName, nil, combineToSpec(readOnlyMappings, readWriteMappings))
}

// dynamicCommand implements the "dynamic" command.
func dynamicCommand(args []string, volumeName string, settings ProfileSettings) error {
	flags := flag.NewFlagSet("dynamic", flag.ExitOnError)
	help := flags.Bool("help", false, "print the usage information and exit")
	input := flags.String("input", "-", "where to read the configuration data from (- for stdin)")
	output := flags.String("output", "-", "where to write the status of reconfiguration to (- for stdout)")

	flags.Usage = func() {} // Suppress default output.
	flags.Parse(args)

	if *help {
		fmt.Fprintf(os.Stdout, "Usage: %s dynamic MOUNT-POINT\n", filepath.Base(os.Args[0]))
		usage(os.Stdout, flags)
		return nil
	}

	if flags.NArg() != 1 {
		return fmt.Errorf("Invalid number of arguments; pass -help flag for details")
	}
	mountPoint := flags.Arg(0)

	dynamicConf := &sandbox.DynamicConf{Input: os.Stdin, Output: os.Stdout}
	if *input != "-" {
		file, err := os.Open(*input)
		if err != nil {
			return fmt.Errorf("Unable to open file %q for reading: %v", *input, err)
		}
		defer file.Close()
		dynamicConf.Input = file
	}
	if *output != "-" {
		file, err := os.Create(*output)
		if err != nil {
			return fmt.Errorf("Unable to open file %q for writing: %v", *output, err)
		}
		defer file.Close()
		dynamicConf.Output = file
	}

	return serve(settings, mountPoint, volumeName, dynamicConf, nil)
}

func serve(settings ProfileSettings, mountPoint string, volumeName string, dynamicConf *sandbox.DynamicConf, mappings []sandbox.MappingSpec) error {
	sfs, err := sandbox.Init(mappings)
	if err != nil {
		return fmt.Errorf("Unable to init sandbox: %v", err)
	}

	profileContext, err := StartProfiling(settings)
	if err != nil {
		return err
	}
	defer profileContext.Close()

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

	mountOk := make(chan string, 1)
	caughtSignal := make(chan os.Signal, 1)
	handleSignals(mountOk, caughtSignal)

	c, err := fuse.Mount(
		mountPoint,
		fuse.FSName("sandboxfs"),
		fuse.Subtype("sandboxfs"),
		fuse.LocalVolume(),
		fuse.VolumeName(volumeName),
	)
	if err != nil {
		mountOk <- "" // Neutralize signal handler.

		// Even if fuse.Mount failed, we can still hit the case where the mount point was
		// registered with the kernel. Try to unmount it here as a best-effort operation.
		// (I.e. we can't tell upfront if the mount point was registered so we have to
		// unconditionally try to unmount it and hope it gets cleaned up.)
		//
		// This was observed to happen on Linux: mounting a FUSE file system requires
		// spawning the fusermount program aside from telling the kernel about the mount
		// point. It can happen that a signal arrives at "the wrong time" within the
		// fuse.Mount call above and we get an error here even when the mount point is left
		// behind. This is likely a bug in the fuse.Mount logic.
		fuse.Unmount(mountPoint)

		return fmt.Errorf("Unable to mount: %v", err)
	}
	defer c.Close()
	mountOk <- mountPoint // Tell signal handler that the mount point requires cleanup.

	err = sandbox.Serve(c, sfs, dynamicConf)
	if err != nil {
		return fmt.Errorf("FileSystem server error: %v", err)
	}

	<-c.Ready
	if err := c.MountError; err != nil {
		return fmt.Errorf("Mount error: %v", err)
	}

	// If we reach this point, the FUSE serve loop has terminated because the user unmounted the
	// file system or because we received a signal. In both cases we need to exit, but we treat
	// signal receipts as an error just so that the user can tell that the exit was not clean.
	select {
	case signal := <-caughtSignal:
		return fmt.Errorf("caught signal: %v", signal.String())
	default:
	}
	return nil
}

func main() {
	cpuProfile := flag.String("cpu_profile", "", "write a CPU profile to the given file on exit")
	debug := flag.Bool("debug", false, "log details about FUSE requests and responses to stderr")
	help := flag.Bool("help", false, "print the usage information and exit")
	listenAddress := flag.String("listen_address", "", "enable HTTP server on the given address and expose pprof data")
	memProfile := flag.String("mem_profile", "", "write a memory profile to the given file on exit")
	volumeName := flag.String("volume_name", "sandbox", "name for the sandboxfs volume")

	flag.Usage = func() {} // Suppress default output.
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

	settings, err := NewProfileSettings(*cpuProfile, *memProfile, *listenAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid profiling settings: %v\n", err)
		os.Exit(1)
	}

	if *debug {
		fuse.Debug = func(msg interface{}) { fmt.Fprintln(os.Stderr, msg) }
	}

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Invalid number of arguments; pass -help flag for details\n")
		os.Exit(1)
	}
	command := flag.Arg(0)
	args := flag.Args()[1:]

	switch command {
	case "static":
		err = staticCommand(args, *volumeName, settings)
	case "dynamic":
		err = dynamicCommand(args, *volumeName, settings)
	default:
		fmt.Fprintf(os.Stderr, "Invalid command; pass -help flag for details\n")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
