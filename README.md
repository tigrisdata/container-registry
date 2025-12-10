# GitLab Container Registry

**Quick Links**:
[**Roadmap**](https://about.gitlab.com/handbook/engineering/development/ops/package/#roadmap) |
[Want to Contribute?](#contributing) |

## Historical Background

The GitLab Container Registry originated as a fork of the
[Docker Distribution Registry](https://github.com/docker-archive/docker-registry),
now [CNCF Distribution](https://github.com/distribution/distribution), both
distributed under [Apache License Version 2.0](LICENSE).

The first GitLab change on top of the upstream implementation was
[efe421fd](https://gitlab.com/gitlab-org/container-registry/-/commit/efe421fde5bfb1312c98863edee25856ba5fd204),
and since then we have diverged enough to the point where we decided
to detach from upstream and proceed on our own path.

Since then, we have implemented and released several major performance
improvements and bug fixes. For a list of changes, please see
[differences from upstream](docs/upstream-differences.md).
These changes culminated on a new architecture based on a relational metadata
database and the original goal, enabling
[online garbage collection](docs/spec/gitlab/online-garbage-collection.md).

## Benchmarking

A benchmark tool is available to measure push/pull performance with realistic Docker-like operations.

### Build

```bash
go build -o bin/benchmark ./cmd/benchmark
```

### Usage

```bash
./bin/benchmark --help

Usage of ./bin/benchmark:
  -chunk-size int
        Chunk size in bytes for chunked uploads (default 5242880 = 5MB)
  -iterations int
        Number of iterations per size (default 3)
  -layers int
        Number of layers per image (default 1)
  -monolithic
        Use monolithic uploads instead of chunked (default: chunked)
  -output string
        Output format: text or json (default "text")
  -registry string
        Target registry URL (default "http://localhost:5000")
  -repository string
        Repository path for test images (default "benchmark/test")
  -sizes string
        Comma-separated layer sizes to test (default "1GB,2GB")
  -tag string
        Tag for manifest (default "benchmark")
```

### How It Works

The benchmark simulates realistic Docker push/pull operations:

- Pushes config blob + layer blobs + manifest
- Pulls manifest + config blob + layer blobs
- Layers are pushed/pulled concurrently (like Docker client)
- Supports chunked (PATCH) or monolithic (PUT) uploads

### Examples

```bash
# Default: 1GB layer, chunked uploads (5MB chunks)
./bin/benchmark -registry http://localhost:5555 -sizes 1GB

# Multi-layer image (3 x 1GB layers = 3GB total)
./bin/benchmark -registry http://localhost:5555 -sizes 1GB -layers 3

# Larger chunks for better throughput
./bin/benchmark -registry http://localhost:5555 -sizes 1GB -chunk-size 52428800

# Monolithic uploads (single PUT per layer)
./bin/benchmark -registry http://localhost:5555 -sizes 1GB -layers 3 -monolithic
```

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for details on how to contribute.
