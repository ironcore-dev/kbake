# kbake

[![REUSE status](https://api.reuse.software/badge/github.com/ironcore-dev/kbake)](https://api.reuse.software/info/github.com/ironcore-dev/ironcore-net)
[![GitHub License](https://img.shields.io/static/v1?label=License&message=Apache-2.0&color=blue)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://makeapullrequest.com)

**kbake** is a declarative Linux kernel builder. You describe the kernel you
want in a `Kernelfile` (YAML) — the source to start from, the config options
and modules to enable or disable, and files to add or remove — and kbake
builds it, reproducibly, for one or more architectures.

The build result is packaged as an [OCI artifact](https://opencontainers.org/)
(a kernel binary plus a compressed `modules.tar.gz`, per architecture) and
stored in a local OCI store, from where it can be tagged, listed, exported or
pulled from a registry.

## Features

- **Declarative**: a single `Kernelfile` describes source, config, modules and
  file changes — think Dockerfile, but for kernels.
- **Multi-arch**: build for several architectures in one invocation; results
  are grouped under a single OCI image index.
- **Reproducible**: the compile phase runs with a pinned build environment
  (`SOURCE_DATE_EPOCH=0`, fixed `KBUILD_BUILD_*` values, `TZ=UTC`, `LC_ALL=C`)
  so identical inputs produce identical artifacts.
- **Fast**: fetched kernel sources are cached go-module-style and shared
  between builds; compilation is accelerated with ccache (enabled by default).
- **Conditional**: per-architecture behavior via a small GitHub-Actions-style
  expression language (`if: ${{ arch == 'arm64' }}`) and `$variable`
  substitution.

## Installation

Requires Go 1.26+.

```console
$ go install github.com/ironcore-dev/kbake@latest
```

Or build from source:

```console
$ git clone https://github.com/ironcore-dev/kbake.git
$ cd kbake
$ make build   # produces bin/kbake
```

## Quickstart

Create a `Kernelfile` (see [examples/Kernelfile](examples/Kernelfile) for a
complete one):

```yaml
from: git://git.kernel.org/pub/scm/linux/kernel/git/stable/linux@v7.1

options:
  enable:
  - EFI
  - DEVTMPFS
  - DEVTMPFS_MOUNT
  - BPF

modules:
  builtin:
  - VIRTIO
  - VIRTIO_NET
  - VIRTIO_BLK
```

Build it for the host architecture and tag the result:

```console
$ kbake build . -t my-kernel:latest
built sha256:9b1d…
tagged my-kernel:latest
```

List stored images and extract the kernel binary:

```console
$ kbake image ls
IMAGE              ID           ARCH
my-kernel:latest   9b1d3f2a1c4  arm64

$ kbake get kernel my-kernel:latest -o kernel
```

To share an image, push the local store's artifacts with your favorite OCI
tooling; `kbake get kernel` can pull them back from any registry (using your
Docker credentials, see below).

## The Kernelfile

A Kernelfile may be YAML or JSON (documents starting with `{` are treated as
JSON). The top-level fields:

| Field     | Description                                                                                           |
|-----------|-------------------------------------------------------------------------------------------------------|
| `from`    | Base source: `scratch` or `git://<host>/<path>@<ref>`.                                                |
| `options` | Shorthand config options to `enable` (=y) / `disable` (=n).                                           |
| `modules` | Shorthand modules to `enable` (=m), make `builtin` (=y) or `disable` (=n).                            |
| `files`   | Shorthand for `add`ing files from the build context into the tree or `delete`ing files from the tree. |
| `actions` | Ordered list of fine-grained actions, optionally guarded by `if` conditions.                          |

### `from`

- `git://<host>/<path-to-repo>@<ref>` — fetch a git repository at the given
  tag, branch or commit, e.g.
  `git://git.kernel.org/pub/scm/linux/kernel/git/stable/linux@v7.1`.
  Fetched trees are cached under `--cache-dir` using a go-module-style layout
  (`<host>/<path>/<repo>@<version>`), so re-builds only pull the delta.
- `scratch` — start from an empty tree.

### Shorthands: `options`, `modules`, `files`

The shorthand fields expand to config changes and file operations applied
before everything in `actions`:

```yaml
options:
  enable:  [BPF, EFI]      # -> CONFIG_BPF=y, CONFIG_EFI=y
  disable: [DEBUG_INFO]    # -> CONFIG_DEBUG_INFO=n

modules:
  enable:  [BRIDGE]        # -> CONFIG_BRIDGE=m
  builtin: [VIRTIO_NET]    # -> CONFIG_VIRTIO_NET=y
  disable: [SOUND]         # -> CONFIG_SOUND=n

files:
- add:   { src: patches/fix.patch, dst: patches/fix.patch }  # from build context into tree
- delete: { path: drivers/staging/rts5208 }                  # remove from tree
```

Symbol names are given without the `CONFIG_` prefix.

### `actions`

For everything the shorthands don't cover, `actions` gives full control.
Each entry — or a whole group of entries — can be conditional:

```yaml
actions:
- if: ${{ arch == 'arm64' }}
  actions:
  - setConfig: { name: ARM64_64K_PAGES, value: Yes }
  - addFile:   { src: firmware/$arch, dst: firmware }
- setConfig:    { name: CMDLINE, value: Yes }       # unconditional
- deleteConfig: { name: WATCHDOG }
```

Available actions: `setConfig` (value `Yes` / `No` / `Module`),
`deleteConfig`, `addFile`, `deleteFile`. File `src` paths are resolved inside
the build context, `dst`/`path` inside the kernel tree; both are protected
against escaping their respective roots.

### Expressions

String fields support `$variable` substitution and `${{ ... }}` expressions,
modeled after GitHub Actions. The build provides the `arch` variable.
Supported operators: `== != < <= > >= && || !` and parentheses; literals:
strings, numbers, `true` / `false` / `null`.

```yaml
actions:
- if: ${{ arch == 'amd64' || arch == 'arm64' }}
  actions:
  - addFile: { src: config/$arch.fragment, dst: .kbake-fragment }
```

## Command reference

```
kbake <command> [flags]
```

Global flag: `--local-dir` — local OCI store (default `~/.kbake/repo`).

### `kbake build CONTEXT [flags]`

Build a kernel from a Kernelfile. `CONTEXT` is the build context directory
used to resolve `files:`/`addFile` sources.

| Flag           | Default                 | Description                                                 |
|----------------|-------------------------|-------------------------------------------------------------|
| `-f, --file`   | `Kernelfile`            | Path to the Kernelfile (`-` reads stdin).                   |
| `--arch`       | host arch               | Comma-separated target architectures (e.g. `amd64,arm64`).  |
| `-t, --tag`    | —                       | Tag(s) to apply to the result in the local store.           |
| `-j, --jobs`   | `-1` (all CPUs)         | Parallel make jobs.                                         |
| `--no-ccache`  | off                     | Disable ccache.                                             |
| `--ccache-dir` | `~/.cache/kbake/ccache` | ccache directory.                                           |
| `--work-dir`   | `~/.cache/kbake/work`   | Per-build work directory root.                              |
| `--keep-work`  | off                     | Keep the work dir after the build (path printed to stderr). |
| `--cache-dir`  | `~/.cache/kbake/mod`    | Kernel source cache (go-style layout).                      |
| `--temp-dir`   | system temp             | Directory for build logs.                                   |

The build runs `defconfig`, applies your config changes, resolves dependencies
with `olddefconfig`, compiles, runs `modules_install` and packs the result as
an OCI artifact with two layers — the kernel binary and `modules.tar.gz` —
per architecture, plus an image index tying the architectures together. The
index digest is printed on success. On failure, the full build log path is
printed to stderr.

### `kbake get kernel IMAGE [flags]`

Extract the kernel binary from an image. Images are resolved from the local
store first; if absent, the image is pulled from the referenced registry
(authenticating via your Docker config, including credential helpers such as
`docker-credential-osxkeychain`).

| Flag           | Default   | Description                                  |
|----------------|-----------|----------------------------------------------|
| `-o, --output` | stdout    | Write to a file instead of stdout.           |
| `-a, --arch`   | host arch | Kernel architecture to extract.              |
| `--plain-http` | off       | Use plain HTTP when pulling from a registry. |

### `kbake image ls`

List images in the local store (tag, short ID, architectures).

### `kbake image rm IMAGE [IMAGE...]`

Remove tags from the local store and garbage-collect unreferenced content.

### `kbake version`

Print the kbake version.

**Exit codes:** `0` success, `1` runtime error (bad Kernelfile, build
failure, …), `2` usage error (unknown command, bad flags).

## Supported architectures

`amd64`, `386`, `arm64`, `arm`, `mips`, `mips64`, `ppc64le`, `riscv64`,
`s390x`.

Native builds use the host toolchain; cross builds expect the matching
`<triplet>-gcc` plus binutils (`as`, `ld`, `ar`, `nm`, `objcopy`, `objdump`,
`strip`) on `PATH` — e.g. `aarch64-linux-gnu-*` for `arm64`. Build host
requirements: `make`, `git`, a C toolchain, and optionally `ccache`.

## OCI layout

Artifacts use dedicated media types:

| Content       | Media type                                            |
|---------------|-------------------------------------------------------|
| Artifact type | `application/vnd.ironcore.kbake.v1+json`              |
| Kernel layer  | `application/vnd.ironcore.kernel.v1`                  |
| Modules layer | `application/vnd.ironcore.kernel.modules.v1.tar+gzip` |
| Config        | `application/vnd.ironcore.kbake.config.v1+json`       |

## Development

```console
$ make build         # build bin/kbake
$ make test          # run tests (with coverage)
$ make lint          # run golangci-lint
$ make check         # add license headers, fmt, lint, test
```

See `make help` for all targets.

## Contributing

We'd love to get feedback from you. Please report bugs, suggestions or post questions by opening a GitHub issue.

## Licensing

Copyright 2025 SAP SE or an SAP affiliate company and IronCore contributors. Please see our [LICENSE](LICENSE) for
copyright and license information. Detailed information including third-party components and their licensing/copyright
information is available [via the REUSE tool](https://api.reuse.software/info/github.com/ironcore-dev/kbake).

<p align="center"><img alt="Bundesministerium für Wirtschaft und Energie (BMWE)-EU funding logo" src="https://apeirora.eu/assets/img/BMWK-EU.png" width="400"/></p>
