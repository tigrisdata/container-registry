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

A benchmark tool is available to measure push/pull performance:

### Build

```bash
go build -o bin/benchmark ./cmd/benchmark
```

### Run (defaults: 1GB,2GB sizes, 3 iterations, localhost:5000)

```bash
./bin/benchmark --help

Usage of ./bin/benchmark:
-iterations int
Number of iterations per size (default 3)
-output string
Output format: text or json (default "text")
-registry string
Target registry URL (default "http://localhost:5000")
-repository string
Repository path for test images (default "benchmark/test")
-sizes string
Comma-separated blob sizes to test (e.g., 1GB,2GB) (default "1GB,2GB")
```

### Custom settings

```bash
./bin/benchmark --registry http://localhost:5555 --sizes 1GB,2GB --iterations 5
```

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for details on how to contribute.
