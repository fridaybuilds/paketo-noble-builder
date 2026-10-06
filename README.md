# Paketo Noble Builder for every language version

A [Cloud Native Buildpacks](https://buildpacks.io) builder for Ubuntu 24.04 (Noble), based on the
[Paketo noble builder](https://github.com/paketo-buildpacks/ubuntu-noble-builder), whose versions together offer every
Ruby, Go, Python, PHP and Node.js version that ever had a noble build in Paketo.

Each Paketo language buildpack release only ships the latest couple of patch versions of each language, so a single
builder can't build an app that pins an older version. Each version of this builder is a snapshot of the Paketo
language buildpacks at a point in time, and the versions are picked so that together they cover every language
version. The builder also adds the [apt buildpack](https://github.com/paketo-buildpacks/apt) to every group, and
[libvips](https://github.com/tashows/paketo-buildpack-vips) for Ruby, Python and Node.js apps that use it, both
optional.

It is built for the fridaybuilds platform, but works with any CNB platform.

## Usage

Find the builder version that has your language version in [SUPPORTED_VERSIONS.md](./SUPPORTED_VERSIONS.md), then:

```bash
pack build my-app --builder docker.io/fridaybuilds/paketo-noble-builder:<version>
```

`latest` is the newest version, with the newest version of every buildpack.

## Versions

Versions are `0.1.<n>`, like the Paketo builders, and never change once published. A new version is published when
anything in the builder changes: a tracked language buildpack, the upstream builder (build image, lifecycle, other
buildpacks) or [overlay.toml](./overlay.toml). The first versions, 0.1.0 to 0.1.12, were computed from the history
of the language buildpacks, and their release notes say which date they are a snapshot of.

Every version is a commit on `main` and a tag with:

- [builder.toml](./builder.toml): the builder of that version,
- [snapshots.json](./snapshots.json): every version up to that one, with the upstream release it is based on, the
  tracked buildpack versions and the language versions they provide,
- [SUPPORTED_VERSIONS.md](./SUPPORTED_VERSIONS.md): every language version, with the newest builder version that has
  it.

Each version also has a [GitHub release](https://github.com/fridaybuilds/paketo-noble-builder/releases) with its
language versions, the changes since the previous version, its buildpacks and its images.

The app's run image, `ubuntu-noble-run:latest`, is picked when the app is built, so apps built with an older builder
version still run on the latest run image.

## How it works

`go run ./cmd/noble-builder resolve` looks up the latest upstream release and walks the release history of the tracked
buildpacks. It keeps the releases whose components all support noble on amd64 and arm64, and adds to `snapshots.json`:

- the fewest points in that history that offer every language version that no published version has,
- and the current state, if its builder differs from the published `builder.toml`.

The [release workflow](./.github/workflows/release.yml) runs it daily and publishes each new version: the builder
image, the commit and tag, and the GitHub release. It then points `latest` at the newest version. It needs a
`DOCKERHUB_TOKEN` secret with write access to the image repository, and optionally a `DOCKERHUB_USERNAME` variable
(`fridaybuilds` by default). The commits are made by `github-actions[bot]`.

## Development

```bash
go test ./...
go run ./cmd/noble-builder resolve -lock /tmp/snapshots.json   # needs network; set GITHUB_TOKEN to avoid rate limits
go run ./cmd/noble-builder                                     # regenerate SUPPORTED_VERSIONS.md
pack builder create my-builder --config builder.toml
```

## License

[Apache License 2.0](./LICENSE). The buildpacks and images this builder is made of keep their own licenses.
