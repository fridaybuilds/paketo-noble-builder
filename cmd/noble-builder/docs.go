package main

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

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

// Notes renders the release notes of lock.Snapshots[i], whose builder is builder. previous is the builder of the
// version before it, if any.
func Notes(overlay Overlay, lock Lock, i int, builder Builder, previous *Builder) []byte {
	snapshot := lock.Snapshots[i]

	var buf bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&buf, format, args...) }

	w("Ubuntu 24.04 (Noble) builder with the Paketo language buildpacks as of %s, based on the Paketo noble builder\n", snapshot.Date)
	w("[%s](https://github.com/%s/releases/tag/%s).\n\n", snapshot.Base, overlay.Base, snapshot.Base)
	w("```bash\npack build my-app --builder %s:%s\n```\n", overlay.Image, snapshot.Version)

	w("\n## Language versions\n\n| Language | Versions |\n| --- | --- |\n")
	for _, language := range languages(overlay) {
		if versions := snapshot.Languages[language]; len(versions) > 0 {
			w("| %s | %s |\n", languageName(language), strings.Join(versions, ", "))
		}
	}

	if previous == nil {
		w("\nThis is the first version.\n")
	} else {
		w("\n## Changes since %s\n\n", lock.Snapshots[i-1].Version)
		changes := languageChanges(overlay, lock.Snapshots[i-1], snapshot)
		changes = append(changes, builderChanges(*previous, builder)...)
		if len(changes) == 0 {
			changes = []string{"No changes."}
		}
		for _, change := range changes {
			w("- %s\n", change)
		}
	}

	w("\n## Buildpacks\n\n| Buildpack | Version |\n| --- | --- |\n")
	for _, entry := range buildpackVersions(builder) {
		optional := ""
		if entry.Optional {
			optional = " (optional)"
		}
		w("| %s%s | %s |\n", entry.ID, optional, entry.Version)
	}

	w("\n## Images\n\n")
	w("- Build image: `%s`\n", builder.Build.Image)
	for _, image := range builder.Run.Images {
		w("- Run image: `%s`\n", image.Image)
	}
	w("- Lifecycle: %s\n", builder.Lifecycle.Version)

	return buf.Bytes()
}

func languageChanges(overlay Overlay, previous, snapshot Snapshot) []string {
	var changes []string
	for _, language := range languages(overlay) {
		var parts []string
		if added := without(snapshot.Languages[language], previous.Languages[language]); len(added) > 0 {
			parts = append(parts, "added "+strings.Join(added, ", "))
		}
		if dropped := without(previous.Languages[language], snapshot.Languages[language]); len(dropped) > 0 {
			parts = append(parts, "dropped "+strings.Join(dropped, ", "))
		}
		if len(parts) > 0 {
			changes = append(changes, fmt.Sprintf("**%s**: %s", languageName(language), strings.Join(parts, "; ")))
		}
	}
	return changes
}

func builderChanges(previous, builder Builder) []string {
	var changes []string
	change := func(what, from, to string) {
		switch {
		case from == to:
		case from == "":
			changes = append(changes, fmt.Sprintf("%s: added %s", what, to))
		case to == "":
			changes = append(changes, fmt.Sprintf("%s: removed (was %s)", what, from))
		default:
			changes = append(changes, fmt.Sprintf("%s: %s → %s", what, from, to))
		}
	}

	versions := func(b Builder) map[string]string {
		m := map[string]string{}
		for _, entry := range buildpackVersions(b) {
			m[entry.ID] = entry.Version
		}
		return m
	}
	before, after := versions(previous), versions(builder)
	for _, entry := range buildpackVersions(builder) {
		change("`"+entry.ID+"`", before[entry.ID], entry.Version)
	}
	for _, entry := range buildpackVersions(previous) {
		if _, ok := after[entry.ID]; !ok {
			change("`"+entry.ID+"`", entry.Version, "")
		}
	}

	change("Build image", imageTag(previous.Build.Image), imageTag(builder.Build.Image))
	change("Lifecycle", previous.Lifecycle.Version, builder.Lifecycle.Version)
	return changes
}

// buildpackVersions returns the buildpacks of the order groups, in order of first appearance.
func buildpackVersions(b Builder) []GroupEntry {
	var entries []GroupEntry
	seen := map[string]bool{}
	for _, order := range b.Order {
		for _, entry := range order.Group {
			if !seen[entry.ID] {
				seen[entry.ID] = true
				entries = append(entries, entry)
			}
		}
	}
	return entries
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
