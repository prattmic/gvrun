// Copyright 2021 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var runscBin = flag.String("runsc", "", "Path to runsc binary")

func run() error {
	// We need a temporary directory for two reasons:
	//
	// 1. We need to place the runtime spec config.json somewhere. OCI
	// requires that we pass the _directory_ containing config.json rather
	// than a path to the config, so we can't even do something clever like
	// donate a pipe and pass /proc/self/fd/N.
	//
	// 2. Below we construct the filesystem as an allowlist by way of bind
	// mounting everything we want to allow over an empty root. OCI strikes
	// again and does not allow simply using a tmpfs as root, it requires a
	// real host path. So we will provide an empty (read-only) directory to
	// act as root.
	dir, err := ioutil.TempDir("", "gvrun")
	if err != nil {
		return fmt.Errorf("error creating temp directory: %v", err)
	}
	defer os.RemoveAll(dir)

	rootPath := filepath.Join(dir, "rootfs")
	if err := os.Mkdir(rootPath, 0755); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}

	specPath := filepath.Join(dir, "config.json")
	specFile, err := os.Create(specPath)
	if err != nil {
		return fmt.Errorf("error creating config.json: %v", err)
	}
	defer specFile.Close()

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("error finding working directory: %v", err)
	}
	log.Printf("Granting read access to %q", wd)

	args := flag.Args()
	binary, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("error computing absolute binary path for %q: %v", args[0], err)
	}
	log.Printf("Granting read access to %q", binary)

	// TODO(prattmic): ask for user confirmation for above access?

	spec := &specs.Spec{
		Process: &specs.Process{
			Args: args,
			Env:  []string{
				"HOME=/",
				"PATH=/usr/local/bin:/usr/bin:/bin",
				"USER=root",
			},
			Cwd: wd,
			Capabilities: nil, // none!
		},
		Hostname: "runsc-gvrun",
		Root: &specs.Root{
			Path: dir,
		},
		Mounts: []specs.Mount{
			// Grant access to the binary and working directory.
			resolvedMount(binary),
			resolvedMount(wd),

			// Important libraries.
			resolvedMount("/lib64/ld-linux-x86-64.so.2"), // dynamic linker.
			resolvedMount("/lib/x86_64-linux-gnu/libc.so.6"), // libc.
			resolvedMount("/lib/x86_64-linux-gnu/libpthread.so.0"), // libpthread.

			resolvedMount("/usr/grte/v4/lib64/ld-linux-x86-64.so.2"), // dynamic linker.
			resolvedMount("/usr/grte/v4/lib64/libc.so.6"), // libc.
			resolvedMount("/usr/grte/v4/lib64/libpthread.so.0"), // libpthread.
		},
	}

	if err := json.NewEncoder(specFile).Encode(spec); err != nil {
		return fmt.Errorf("error writing config.json: %v", err)
	}

	cmd := exec.Command(*runscBin)
	// Write to in-memory overlayfs, not host.
	cmd.Args = append(cmd.Args, "--overlay")
	// No networking.
	cmd.Args = append(cmd.Args, "--network=none")

	// Debugging.
	cmd.Args = append(cmd.Args, "-debug")
	cmd.Args = append(cmd.Args, "-debug-log=/tmp/")

	cmd.Args = append(cmd.Args, "run")
	// Spec location.
	cmd.Args = append(cmd.Args, "--bundle", dir)
	// Container name.
	//
	// TODO(prattmic): make unique?
	cmd.Args = append(cmd.Args, "gvrun")

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command failed: %v\n", err)
	}
	return nil
}

// resolvedMount returns a bind mount for path that points to the actual
// location of path (resolving any symlinks). This avoids the need to also
// mount all the symlinks along the way.
func resolvedMount(path string) specs.Mount {
	// Resolve final location of path.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// TODO(prattmic): return error.
		panic(fmt.Sprintf("failed to resolve symlinks of %q: %v", path, err))
	}

	return specs.Mount{
		Type:        "bind",
		Destination: path,
		Source:      resolved,
	}
}

func main() {
	flag.Parse()

	if len(flag.Args()) == 0 {
		log.Fatal("Program to run required!")
	}

	if *runscBin == "" {
		log.Fatal("Path to runsc required via -runsc")
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
}
