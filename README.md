# Paketo Noble Builder for every language version

[Cloud Native Buildpacks](https://buildpacks.io) builders for Ubuntu 24.04 (Noble), based on the
[Paketo noble builder](https://github.com/paketo-buildpacks/ubuntu-noble-builder), that together offer every Ruby,
Go, Python, PHP and Node.js version that ever had a noble build in Paketo.

Each Paketo language buildpack release only ships the latest couple of patch versions of each language, so a single
builder can't build an app that pins an older version. These builders are snapshots of the Paketo language buildpacks
at the points in time needed to cover every version. They also add the
[apt buildpack](https://github.com/paketo-buildpacks/apt) to every group, and
[libvips](https://github.com/tashows/paketo-buildpack-vips) for Ruby, Python and Node.js apps that use it, both
optional.

They are built for the fridaybuilds platform, but work with any CNB platform.

## Usage

Find the image tag that has your language version in [SUPPORTED_VERSIONS.md](./SUPPORTED_VERSIONS.md), then:

```bash
pack build my-app --builder docker.io/fridaybuilds/paketo-noble-builder:2026.10.02
```

`latest` is the newest snapshot, with the newest version of every buildpack.

## Tags

| Tag | Example | Meaning |
| --- | --- | --- |
| `<snapshot>-r<N>` | `2026.10.02-r1` | Immutable. One revision of a snapshot. |
| `<snapshot>` | `2026.10.02` | The newest revision of a snapshot. |
| `latest` | | The newest revision of the newest snapshot. |

A snapshot is named after the date of the Paketo releases it is made of, and pins the Ruby, Go, Python, PHP and
Node.js buildpack versions of that date. Everything else (build image, lifecycle, the Java, .NET, web servers and
Procfile buildpacks, apt and vips) comes from the latest upstream builder, so snapshots keep getting security
updates: whenever that changes, every snapshot gets a new revision.

Each snapshot has a [GitHub release](https://github.com/fridaybuilds/paketo-noble-builder/releases) that lists its
language versions, the changes since the previous snapshot, its buildpacks and its revisions, with the exact
`builder-r<N>.toml` of every revision attached.

## How it works

- [overlay.toml](./overlay.toml) is what these builders add to the upstream builder: which stacks have their
  versions tracked, and the buildpacks added to the order groups.
- [snapshots.json](./snapshots.json) pins the upstream release the builders are based on, and lists the snapshots:
  the tracked buildpack versions of each, and the language versions they provide.
- `go run ./cmd/noble-builder resolve` moves `snapshots.json` to the latest upstream release and walks the release
  history of the tracked buildpacks. It keeps the releases whose components all support noble on amd64 and arm64,
  and picks the fewest points in time at which, together, every language version was available. Snapshots that are
  already in `snapshots.json` are kept, so a tag never changes meaning.
- The builder.toml of a snapshot is generated from the upstream builder.toml and the overlay
  (`go run ./cmd/noble-builder builder -snapshot <name>`), and isn't kept in the repository.

### Workflows

- **Resolve** runs daily and opens a pull request when `snapshots.json` changes.
- **Release** runs when `snapshots.json` or `overlay.toml` change on `main`. It publishes a new revision of every
  snapshot whose builder.toml differs from its latest revision, updates the releases, moves `latest`, and attaches the
  published `snapshots.json` to the latest release
  (`https://github.com/fridaybuilds/paketo-noble-builder/releases/latest/download/snapshots.json`).
- **Test** runs the tests and checks that `SUPPORTED_VERSIONS.md` is up to date.

Publishing needs a `DOCKERHUB_TOKEN` secret with write access to the image repository, and optionally a
`DOCKERHUB_USERNAME` variable (`fridaybuilds` by default). The resolve workflow needs "Allow GitHub Actions to create
and approve pull requests" enabled in the repository's Actions settings.

## Development

```bash
go test ./...
go run ./cmd/noble-builder resolve   # needs network; set GITHUB_TOKEN to avoid GitHub's API rate limits
go run ./cmd/noble-builder           # regenerate SUPPORTED_VERSIONS.md after changing the overlay
go run ./cmd/noble-builder builder -snapshot 2026.10.02 > builder.toml
pack builder create my-builder --config builder.toml
```

## License

[Apache License 2.0](./LICENSE). The buildpacks and images these builders are made of keep their own licenses.
