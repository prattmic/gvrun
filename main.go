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
	"strconv"
	"strings"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	runscBin = flag.String("runsc", "", "Path to runsc binary")

	extraEnv  = flag.String("extra_env", "", "Comma-separated list of environment variables to set")
	extraDirs = flag.String("extra_dirs", "", "Comma-separated list of extra directories (or files) to provide read-only access to")
)

// originalUser return the uid, gid, and username of the user that invoked this
// binary. Note that this must be invoked under sudo, so this is the user
// invoking sudo, not root.
func originalUser() (uint32, uint32, string, error) {
	uidString, ok := os.LookupEnv("SUDO_UID")
	if !ok {
		return 0, 0, "", fmt.Errorf("unable to determine original uid; did you run under sudo?")
	}
	uid, err := strconv.ParseUint(uidString, 10, 32)
	if err != nil {
		return 0, 0, "", fmt.Errorf("error converting %q to uint32: %v", uidString, err)
	}

	gidString, ok := os.LookupEnv("SUDO_GID")
	if !ok {
		return 0, 0, "", fmt.Errorf("unable to determine original gid; did you run under sudo?")
	}
	gid, err := strconv.ParseUint(gidString, 10, 32)
	if err != nil {
		return 0, 0, "", fmt.Errorf("error converting %q to uint32: %v", gidString, err)
	}

	name, ok := os.LookupEnv("SUDO_USER")
	if !ok {
		return 0, 0, "", fmt.Errorf("unable to determine original username; did you run under sudo?")
	}

	return uint32(uid), uint32(gid), name, nil
}

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

	// We pretend to be the current host user. This simplifies file access
	// (files are often accessible only by this user), but we should
	// consider locking this down more.
	uid, gid, username, err := originalUser()
	if err != nil {
		return fmt.Errorf("error determining user: %v", err)
	}

	spec := &specs.Spec{
		Process: &specs.Process{
			Args: args,
			Env: []string{
				"HOME=/tmp",
				"PATH=/usr/local/bin:/usr/bin:/bin",
				"USER=" + username,
			},
			Cwd: wd,
			User: specs.User{
				UID:      uid,
				GID:      gid,
				Username: username,
			},
			Capabilities: nil, // none!
		},
		Hostname: "runsc-gvrun",
		Root: &specs.Root{
			Path: rootPath,
		},
		Mounts: []specs.Mount{
			// Grant access to the binary and working directory.
			resolvedMount(binary),
			resolvedMount(wd),

			// Important libraries.
			resolvedMount("/lib64/ld-linux-x86-64.so.2"),           // dynamic linker.
			resolvedMount("/lib/x86_64-linux-gnu/libc.so.6"),       // libc.
			resolvedMount("/lib/x86_64-linux-gnu/libpthread.so.0"), // libpthread.

			resolvedMount("/usr/grte/v4/lib64/ld-linux-x86-64.so.2"), // dynamic linker.
			resolvedMount("/usr/grte/v4/lib64/libc.so.6"),            // libc.
			resolvedMount("/usr/grte/v4/lib64/libpthread.so.0"),      // libpthread.
		},
	}

	if *extraEnv != "" {
		env := strings.Split(*extraEnv, ",")
		for _, e := range env {
			spec.Process.Env = append(spec.Process.Env, e)
		}
	}

	if *extraDirs != "" {
		dirs := strings.Split(*extraDirs, ",")
		for _, d := range dirs {
			log.Printf("Granting read access to %q", d)
			spec.Mounts = append(spec.Mounts, resolvedMount(d))
		}
	}

	if err := json.NewEncoder(specFile).Encode(spec); err != nil {
		return fmt.Errorf("error writing config.json: %v", err)
	}

	// sudo lowers RLIMIT_NOFILE to 1024 by default, which runsc and the
	// sandboxed application will inherit. Raise it back up to a more
	// reasonable level.
	const fileLimit = 32768
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("error getting rlimit: %v", err)
	}
	if limit.Max < fileLimit {
		return fmt.Errorf("file limit too low: %+v", limit)
	}
	limit.Cur = fileLimit
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("error setting rlimit: %v", err)
	}

	cmd := exec.Command(*runscBin)
	// Write to in-memory overlayfs, not host.
	cmd.Args = append(cmd.Args, "--overlay")
	// No networking.
	cmd.Args = append(cmd.Args, "--network=none")

	// Debugging.
	// cmd.Args = append(cmd.Args, "--strace")
	// cmd.Args = append(cmd.Args, "--debug")
	// cmd.Args = append(cmd.Args, "--debug-log=/tmp/")

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
		log.Fatalf("usage: sudo %s -runsc program args...", os.Args[0])
	}

	if *runscBin == "" {
		log.Fatalf("usage: sudo %s -runsc program args...", os.Args[0])
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
}
