# `gvrun`: simple sandboxing with gVisor

`gvrun` is a rudimentary wrapper around [gVisor's](http://gvisor.dev) `runsc`
that allows simple sandboxing of local workloads without a container image.

`gvrun` is intended only for running very simple workloads. Workloads running in
`gvrun` are given access only to the binary itself, the current working
directory, and a few critical system libraries (like libc). As a result, many
workloads will not work out-of-the-box with `gvrun`.

Workloads have no host filesystem write access (all writes are in-memory only)
and no network access.

## Getting Started

1. Build `gvrun` with `go build`.

2. [Download](https://gvisor.dev/docs/user_guide/install/) or build a copy of
   `runsc`. Note that only the `runsc` binary is required, not any Docker or
   containerd configuration.

3. Run a workload: `sudo /path/to/gvrun -runsc /path/to/runsc /bin/echo hello world`.

Note that `gvrun` must be run with `sudo`, as gVisor requires root permissions
to set up the sandbox.
