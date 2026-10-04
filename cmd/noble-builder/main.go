// Command noble-builder manages the fridaybuilds noble builders: the snapshots in snapshots.json and the builder.toml,
// release notes and docs generated from them. Run it from the repository root.
//
//	noble-builder [-check]                     write SUPPORTED_VERSIONS.md, or check that it is up to date
//	noble-builder resolve [-v]                 update the upstream release and the snapshots, then write the docs
//	noble-builder builder -snapshot <name>     print the builder.toml of a snapshot
//	noble-builder notes -snapshot <name> [-revisions <file>]
//	                                           print the release notes of a snapshot
//	noble-builder revision -snapshot <name> -number <n> -commit <sha> -digest <digest>
//	                                           print the revisions.json entry of a new revision of a snapshot
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	overlayFile   = "overlay.toml"
	lockFile      = "snapshots.json"
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
	name := flags.String("snapshot", "", "snapshot name")
	revisionsPath := flags.String("revisions", "", "revisions.json of the snapshot's release")
	number := flags.Int("number", 0, "revision number")
	commit := flags.String("commit", "", "commit the revision is built from")
	digest := flags.String("digest", "", "digest of the revision's image")
	_ = flags.Parse(args)

	var overlay Overlay
	if err := decodeStrict(overlayFile, &overlay); err != nil {
		return err
	}

	lock, err := readLock()
	if err != nil {
		return err
	}

	resolver, err := NewResolver()
	if err != nil {
		return err
	}
	resolver.Verbose = *verbose

	switch command {
	case "resolve":
		if lock, err = resolve(resolver, overlay, lock); err != nil {
			return err
		}
		if err := writeJSON(lockFile, lock); err != nil {
			return err
		}
		fmt.Printf("wrote %s: %d snapshots on %s %s\n", lockFile, len(lock.Snapshots), overlay.Base, lock.Base)
		return writeSupportedVersions(overlay, lock, false)

	case "generate":
		if _, err := baseBuilder(resolver, overlay, lock); err != nil {
			return err
		}
		return writeSupportedVersions(overlay, lock, *check)

	case "builder", "notes", "revision":
		snapshot, err := findSnapshot(lock, *name)
		if err != nil {
			return err
		}
		base, err := baseBuilder(resolver, overlay, lock)
		if err != nil {
			return err
		}
		builder, err := Generate(base, overlay, &snapshot)
		if err != nil {
			return err
		}

		switch command {
		case "builder":
			_, err = os.Stdout.Write(Encode(builder))
			return err

		case "notes":
			var revisions []Revision
			if *revisionsPath != "" {
				if err := readJSON(*revisionsPath, &revisions); err != nil {
					return err
				}
			}
			notes, err := Notes(overlay, lock, snapshot.Name, builder, revisions)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(notes)
			return err

		default:
			if *number < 1 || *commit == "" || *digest == "" {
				return errors.New("revision needs -number, -commit and -digest")
			}
			return json.NewEncoder(os.Stdout).Encode(Revision{
				Revision:   *number,
				Date:       time.Now().UTC().Format("2006-01-02"),
				Base:       lock.Base,
				BuildImage: builder.Build.Image,
				Lifecycle:  builder.Lifecycle.Version,
				Commit:     *commit,
				Digest:     *digest,
			})
		}

	default:
		return errors.New("usage: noble-builder [resolve|builder|notes|revision] [flags]")
	}
}

// resolve moves the lock to the latest upstream release and picks the snapshots from the tracked stacks' release
// history, keeping the snapshots that are already in the lock.
func resolve(resolver *Resolver, overlay Overlay, lock Lock) (Lock, error) {
	tag, err := resolver.LatestRelease(overlay.Base)
	if err != nil {
		return Lock{}, err
	}
	lock.Base = tag

	base, err := baseBuilder(resolver, overlay, lock)
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

	lock.Snapshots = MergeSnapshots(lock.Snapshots, Snapshots(stacks, tracks))

	// Every language version of every release should be in a snapshot, otherwise the snapshot picking is broken.
	covered := map[string]bool{}
	for _, snapshot := range lock.Snapshots {
		for language, versions := range snapshot.Languages {
			for _, version := range versions {
				covered[language+"@"+version] = true
			}
		}
	}
	for stack, releases := range stacks {
		for _, release := range releases {
			for _, version := range release.Languages {
				if !covered[tracks[stack]+"@"+version] {
					return Lock{}, fmt.Errorf("%s %s of %s %s isn't in any snapshot", tracks[stack], version, stack, release.Version)
				}
			}
		}
	}

	return lock, nil
}

// baseBuilder fetches the upstream builder.toml at the lock's release and checks the overlay against it.
func baseBuilder(resolver *Resolver, overlay Overlay, lock Lock) (Builder, error) {
	if lock.Base == "" {
		return Builder{}, fmt.Errorf("%s has no base release, run `go run ./cmd/noble-builder resolve`", lockFile)
	}

	content, err := resolver.FetchFile(overlay.Base, lock.Base, overlay.BasePath)
	if err != nil {
		return Builder{}, fmt.Errorf("fetching %s %s %s: %w", overlay.Base, lock.Base, overlay.BasePath, err)
	}

	var base Builder
	meta, err := toml.Decode(string(content), &base)
	if err != nil {
		return Builder{}, fmt.Errorf("decoding %s %s %s: %w", overlay.Base, lock.Base, overlay.BasePath, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Builder{}, fmt.Errorf("%s %s %s has unsupported keys %v, add them to cmd/noble-builder", overlay.Base, lock.Base, overlay.BasePath, undecoded)
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

func findSnapshot(lock Lock, name string) (Snapshot, error) {
	if name == "" {
		return Snapshot{}, errors.New("-snapshot is required")
	}
	i := slices.IndexFunc(lock.Snapshots, func(s Snapshot) bool { return s.Name == name })
	if i < 0 {
		return Snapshot{}, fmt.Errorf("no snapshot %s in %s", name, lockFile)
	}
	return lock.Snapshots[i], nil
}

func readLock() (Lock, error) {
	var lock Lock
	err := readJSON(lockFile, &lock)
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
