// Command noble-builder manages the fridaybuilds noble builder versions. Run it from the repository root.
//
// snapshots.json lists the published versions. builder.toml, snapshots.json and SUPPORTED_VERSIONS.md at the root are
// those of the newest one, and every version is a commit and a tag with them.
//
//	noble-builder resolve [-lock <file>] [-v]   add the versions to publish to snapshots.json (or write it to <file>)
//	noble-builder prepare -lock <file> -version <version>
//	                                            write the root files of a version from a resolved snapshots.json
//	noble-builder notes -lock <file> -version <version> [-builder <builder.toml>] [-previous <builder.toml>]
//	                                            print the release notes of a version
//	noble-builder [-check]                      write SUPPORTED_VERSIONS.md, or check that it is up to date
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/BurntSushi/toml"
)

const (
	overlayFile   = "overlay.toml"
	lockFile      = "snapshots.json"
	builderFile   = "builder.toml"
	supportedFile = "SUPPORTED_VERSIONS.md"
)

func main() {
	command := "generate"
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}

	if err := run(command, args); err != nil {
		fmt.Fprintln(os.Stderr, "noble-builder:", err)
		os.Exit(1)
	}
}

func run(command string, args []string) error {
	flags := flag.NewFlagSet("noble-builder "+command, flag.ExitOnError)
	check := flags.Bool("check", false, "only check that the generated files are up to date")
	verbose := flags.Bool("v", false, "log why releases are skipped when resolving")
	lockPath := flags.String("lock", lockFile, "snapshots.json with the versions to publish")
	version := flags.String("version", "", "version")
	builderPath := flags.String("builder", "", "builder.toml of the version, e.g. from its tag (notes)")
	previousPath := flags.String("previous", "", "builder.toml of the previous version, e.g. from its tag (notes)")
	_ = flags.Parse(args)

	var overlay Overlay
	if err := decodeStrict(overlayFile, &overlay); err != nil {
		return err
	}
	if overlay.Series == "" {
		return fmt.Errorf("%s has no series", overlayFile)
	}

	resolver, err := NewResolver()
	if err != nil {
		return err
	}
	resolver.Verbose = *verbose

	switch command {
	case "resolve":
		lock, err := readLock(lockFile)
		if err != nil {
			return err
		}
		published := len(lock.Snapshots)
		if lock, err = resolve(resolver, overlay, lock); err != nil {
			return err
		}
		for _, snapshot := range lock.Snapshots[published:] {
			fmt.Printf("new version %s: %s on %s %s\n", snapshot.Version, snapshot.Date, overlay.Base, snapshot.Base)
		}
		return writeJSON(*lockPath, lock)

	case "prepare", "notes":
		lock, err := readLock(*lockPath)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(lock.Snapshots, func(s Snapshot) bool { return s.Version == *version })
		if i < 0 {
			return fmt.Errorf("no version %q in %s", *version, *lockPath)
		}
		builders, err := snapshotBuilders(resolver, overlay, lock.Snapshots[max(i-1, 0):i+1])
		if err != nil {
			return err
		}
		builder := builders[len(builders)-1]

		if command == "notes" {
			// The published builders are the ones in the tags; regenerating them would use today's overlay.
			if *builderPath != "" {
				if _, err := toml.DecodeFile(*builderPath, &builder); err != nil {
					return fmt.Errorf("decoding %s: %w", *builderPath, err)
				}
			}
			var previous *Builder
			if *previousPath != "" {
				previous = &Builder{}
				if _, err := toml.DecodeFile(*previousPath, previous); err != nil {
					return fmt.Errorf("decoding %s: %w", *previousPath, err)
				}
			} else if i > 0 {
				previous = &builders[0]
			}
			_, err := os.Stdout.Write(Notes(overlay, lock, i, builder, previous))
			return err
		}

		published := Lock{Snapshots: lock.Snapshots[:i+1]}
		if err := os.WriteFile(builderFile, Encode(builder), 0o644); err != nil {
			return err
		}
		if err := writeJSON(lockFile, published); err != nil {
			return err
		}
		return os.WriteFile(supportedFile, SupportedVersions(overlay, published), 0o644)

	case "generate":
		lock, err := readLock(lockFile)
		if err != nil {
			return err
		}
		if len(lock.Snapshots) == 0 {
			return nil // nothing is published yet
		}
		if _, err := baseBuilder(resolver, overlay, lock.Snapshots[len(lock.Snapshots)-1].Base); err != nil {
			return err
		}
		return writeSupportedVersions(overlay, lock, *check)

	default:
		return errors.New("usage: noble-builder [resolve|prepare|notes] [flags]")
	}
}

// resolve adds the versions to publish to the lock, based on the latest upstream release:
//   - the fewest points in the tracked stacks' release history that offer every language version that no published
//     version has (on the first run, these backfill the whole history),
//   - and the current state, if its builder differs from the newest published one (builder.toml), e.g. because of a
//     new upstream release or an overlay change.
func resolve(resolver *Resolver, overlay Overlay, lock Lock) (Lock, error) {
	tag, err := resolver.LatestRelease(overlay.Base)
	if err != nil {
		return Lock{}, err
	}
	base, err := baseBuilder(resolver, overlay, tag)
	if err != nil {
		return Lock{}, err
	}

	resolver.StackID = base.Stack.ID
	resolver.Arches = nil
	for _, target := range base.Targets {
		if len(overlay.Arches) == 0 || slices.Contains(overlay.Arches, target.Arch) {
			resolver.Arches = append(resolver.Arches, target.Arch)
		}
	}

	stacks := map[string][]StackRelease{}
	tracks := map[string]string{}
	for _, stack := range overlay.Stacks {
		if stack.Track == "" {
			continue
		}

		releases, err := resolver.StackReleases(stack.ID, stack.Track)
		if err != nil {
			return Lock{}, err
		}
		if len(releases) == 0 {
			fmt.Printf("%s: no release supports %s, leaving it out\n", stack.ID, base.Stack.ID)
			continue
		}

		versions := map[string]bool{}
		for _, release := range releases {
			for _, version := range release.Languages {
				versions[version] = true
			}
		}
		oldest := releases[len(releases)-1]
		fmt.Printf("%s: %d releases since %s (%s), %d %s versions\n", stack.ID, len(releases),
			oldest.Version, oldest.Published.Format("2006-01-02"), len(versions), stack.Track)

		stacks[stack.ID] = releases
		tracks[stack.ID] = stack.Track
	}

	states := Timeline(stacks, tracks)
	if len(states) == 0 {
		return lock, nil
	}

	covered := map[string]bool{}
	for _, snapshot := range lock.Snapshots {
		for language, versions := range snapshot.Languages {
			for _, version := range versions {
				covered[language+"@"+version] = true
			}
		}
	}

	picked := PickStates(states, covered)
	current := len(states) - 1
	if !slices.Contains(picked, current) {
		changed, err := builderChanged(base, overlay, states[current])
		if err != nil {
			return Lock{}, err
		}
		if changed {
			picked = append(picked, current)
		}
	}

	for _, i := range picked {
		lock.Snapshots = append(lock.Snapshots, Snapshot{
			Version:    NextVersion(overlay.Series, lock.Snapshots),
			Date:       states[i].Date.UTC().Format("2006-01-02"),
			Base:       tag,
			Buildpacks: states[i].Buildpacks,
			Languages:  states[i].Languages,
		})
	}

	// Every language version in the history should be in a version, otherwise the picking is broken.
	for _, snapshot := range lock.Snapshots {
		for language, versions := range snapshot.Languages {
			for _, version := range versions {
				covered[language+"@"+version] = true
			}
		}
	}
	for _, state := range states {
		for language, versions := range state.Languages {
			for _, version := range versions {
				if !covered[language+"@"+version] {
					return Lock{}, fmt.Errorf("%s %s isn't in any version", language, version)
				}
			}
		}
	}

	return lock, nil
}

// builderChanged tells whether the builder of a state differs from the newest published one.
func builderChanged(base Builder, overlay Overlay, state State) (bool, error) {
	builder, err := Generate(base, overlay, &Snapshot{Buildpacks: state.Buildpacks})
	if err != nil {
		return false, err
	}

	published, err := os.ReadFile(builderFile)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !bytes.Equal(published, Encode(builder)), nil
}

// snapshotBuilders generates the builders of the snapshots, each on its own upstream release.
func snapshotBuilders(resolver *Resolver, overlay Overlay, snapshots []Snapshot) ([]Builder, error) {
	var builders []Builder
	for _, snapshot := range snapshots {
		base, err := baseBuilder(resolver, overlay, snapshot.Base)
		if err != nil {
			return nil, err
		}
		builder, err := Generate(base, overlay, &snapshot)
		if err != nil {
			return nil, err
		}
		builders = append(builders, builder)
	}
	return builders, nil
}

// baseBuilder fetches the upstream builder.toml at a release and checks the overlay against it.
func baseBuilder(resolver *Resolver, overlay Overlay, tag string) (Builder, error) {
	content, err := resolver.FetchFile(overlay.Base, tag, overlay.BasePath)
	if err != nil {
		return Builder{}, fmt.Errorf("fetching %s %s %s: %w", overlay.Base, tag, overlay.BasePath, err)
	}

	var base Builder
	meta, err := toml.Decode(string(content), &base)
	if err != nil {
		return Builder{}, fmt.Errorf("decoding %s %s %s: %w", overlay.Base, tag, overlay.BasePath, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Builder{}, fmt.Errorf("%s %s %s has unsupported keys %v, add them to cmd/noble-builder", overlay.Base, tag, overlay.BasePath, undecoded)
	}

	return base, validate(base, overlay)
}

func writeSupportedVersions(overlay Overlay, lock Lock, check bool) error {
	content := SupportedVersions(overlay, lock)
	existing, err := os.ReadFile(supportedFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bytes.Equal(existing, content) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is out of date, run `go run ./cmd/noble-builder`", supportedFile)
	}
	fmt.Println("wrote", supportedFile)
	return os.WriteFile(supportedFile, content, 0o644)
}

func readLock(path string) (Lock, error) {
	var lock Lock
	err := readJSON(path, &lock)
	if errors.Is(err, os.ErrNotExist) {
		return Lock{}, nil
	}
	return lock, err
}

func readJSON(path string, v any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, v); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	content, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}

// decodeStrict fails on keys the structs don't know about, so typos and new fields aren't silently ignored.
func decodeStrict(path string, v any) error {
	meta, err := toml.DecodeFile(path, v)
	if err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return fmt.Errorf("decoding %s: unsupported keys %v", path, undecoded)
	}
	return nil
}
