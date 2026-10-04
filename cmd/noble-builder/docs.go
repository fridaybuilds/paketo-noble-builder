package main

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Revision is one immutable image of a snapshot. The release workflow keeps them in the revisions.json asset of the
// snapshot's release.
type Revision struct {
	Revision   int    `json:"revision"`
	Date       string `json:"date"`
	Base       string `json:"base"`
	BuildImage string `json:"build_image"`
	Lifecycle  string `json:"lifecycle"`
	Commit     string `json:"commit"`
	Digest     string `json:"digest"`
}

var languageNames = map[string]string{
	"go":     "Go",
	"node":   "Node.js",
	"php":    "PHP",
	"python": "Python",
	"ruby":   "Ruby",
}

func languageName(language string) string {
	if name, ok := languageNames[language]; ok {
		return name
	}
	return language
}

// languages returns the tracked languages in the order of the overlay.
func languages(overlay Overlay) []string {
	var tracked []string
	for _, stack := range overlay.Stacks {
		if stack.Track != "" {
			tracked = append(tracked, stack.Track)
		}
	}
	return tracked
}

// SupportedVersions renders SUPPORTED_VERSIONS.md: for every language version, the newest snapshot that has it.
func SupportedVersions(overlay Overlay, lock Lock) []byte {
	var buf bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&buf, format, args...) }

	w("<!-- Generated from snapshots.json by `go run ./cmd/noble-builder`. DO NOT EDIT. -->\n\n")
	w("# Supported language versions\n\n")
	w("Each language version is listed with the newest image tag that has it, to use as\n")
	w("`%s:<tag>`. Older patch versions are kept for apps that pin them, but upgrading to the newest patch\n", overlay.Image)
	w("of a minor version is recommended.\n")

	for _, language := range languages(overlay) {
		newest := map[string]string{}
		for _, snapshot := range lock.Snapshots {
			for _, version := range snapshot.Languages[language] {
				newest[version] = snapshot.Name
			}
		}
		if len(newest) == 0 {
			continue
		}

		versions := slices.Collect(maps.Keys(newest))
		sortVersions(versions)
		slices.Reverse(versions)

		w("\n## %s\n\n| Version | Image tag |\n| --- | --- |\n", languageName(language))
		for _, version := range versions {
			w("| %s | `%s` |\n", version, newest[version])
		}
	}

	return buf.Bytes()
}

// Notes renders the release notes of a snapshot. builder is the snapshot's builder as of its newest revision.
func Notes(overlay Overlay, lock Lock, name string, builder Builder, revisions []Revision) ([]byte, error) {
	i := slices.IndexFunc(lock.Snapshots, func(s Snapshot) bool { return s.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("no snapshot %s", name)
	}
	snapshot := lock.Snapshots[i]

	var buf bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&buf, format, args...) }

	w("Ubuntu 24.04 (Noble) builder with the Paketo language buildpacks as they were on %s.\n\n", strings.ReplaceAll(strings.Split(name, "-")[0], ".", "-"))
	w("```bash\npack build my-app --builder %s:%s\n```\n", overlay.Image, name)

	w("\n## Language versions\n\n| Language | Versions |\n| --- | --- |\n")
	for _, language := range languages(overlay) {
		if versions := snapshot.Languages[language]; len(versions) > 0 {
			w("| %s | %s |\n", languageName(language), strings.Join(versions, ", "))
		}
	}

	if i == 0 {
		w("\nThis is the first snapshot.\n")
	} else {
		previous := lock.Snapshots[i-1]
		w("\n### Changes since %s\n\n", previous.Name)
		changed := false
		for _, language := range languages(overlay) {
			added := without(snapshot.Languages[language], previous.Languages[language])
			dropped := without(previous.Languages[language], snapshot.Languages[language])
			var changes []string
			if len(added) > 0 {
				changes = append(changes, "added "+strings.Join(added, ", "))
			}
			if len(dropped) > 0 {
				changes = append(changes, "dropped "+strings.Join(dropped, ", "))
			}
			if len(changes) > 0 {
				w("- **%s**: %s\n", languageName(language), strings.Join(changes, "; "))
				changed = true
			}
		}
		if !changed {
			w("No language versions changed.\n")
		}
	}

	w("\n## Buildpacks\n\n| Buildpack | Version |\n| --- | --- |\n")
	seen := map[string]bool{}
	for _, order := range builder.Order {
		for _, entry := range order.Group {
			if !seen[entry.ID] {
				seen[entry.ID] = true
				optional := ""
				if entry.Optional {
					optional = " (optional)"
				}
				w("| %s%s | %s |\n", entry.ID, optional, entry.Version)
			}
		}
	}

	if len(revisions) > 0 {
		w("\n## Revisions\n\n")
		w("Each revision is an immutable image tag. `%s` always points at the newest revision; it gets a new one when the\n", name)
		w("upstream builder (build image, lifecycle, other buildpacks) or the overlay changes. The `builder-r<N>.toml`\n")
		w("assets are the exact builder configuration of each revision.\n\n")
		w("| Tag | Date | Upstream | Build image | Lifecycle | Digest |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, revision := range slices.Backward(revisions) {
			w("| `%s-r%d` | %s | [%s](https://github.com/%s/releases/tag/%s) | %s | %s | `%s` |\n",
				name, revision.Revision, revision.Date, revision.Base, overlay.Base, revision.Base,
				imageTag(revision.BuildImage), revision.Lifecycle, revision.Digest)
		}
	}

	return buf.Bytes(), nil
}

func without(versions, others []string) []string {
	var result []string
	for _, version := range versions {
		if !slices.Contains(others, version) {
			result = append(result, version)
		}
	}
	return result
}

func imageTag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return image
}
