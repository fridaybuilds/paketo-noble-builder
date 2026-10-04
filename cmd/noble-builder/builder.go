package main

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Builder is a builder.toml, with the fields the upstream builder uses.
type Builder struct {
	Description string      `toml:"description"`
	Build       Build       `toml:"build"`
	Buildpacks  []Buildpack `toml:"buildpacks"`
	Lifecycle   Lifecycle   `toml:"lifecycle"`
	Order       []Order     `toml:"order"`
	Run         Run         `toml:"run"`
	Stack       Stack       `toml:"stack"`
	Targets     []Target    `toml:"targets"`
}

type Build struct {
	Image string `toml:"image"`
}

type Buildpack struct {
	URI     string `toml:"uri"`
	Version string `toml:"version"`
}

type Lifecycle struct {
	Version string `toml:"version"`
}

type Order struct {
	Group []GroupEntry `toml:"group"`
}

type GroupEntry struct {
	ID       string `toml:"id"`
	Version  string `toml:"version"`
	Optional bool   `toml:"optional"`
}

type Run struct {
	Images []RunImage `toml:"images"`
}

type RunImage struct {
	Image string `toml:"image"`
}

type Stack struct {
	ID string `toml:"id"`
}

type Target struct {
	Arch string `toml:"arch"`
	OS   string `toml:"os"`
}

// Overlay is overlay.toml: what this builder adds to the upstream one.
type Overlay struct {
	Base        string         `toml:"base"`
	BasePath    string         `toml:"base_path"`
	Image       string         `toml:"image"`
	Description string         `toml:"description"`
	Arches      []string       `toml:"arches"`
	Stacks      []OverlayStack `toml:"stacks"`
	Prepend     []Prepend      `toml:"prepend"`
}

type OverlayStack struct {
	ID       string `toml:"id"`
	Image    string `toml:"image"`
	Track    string `toml:"track"`
	Version  string `toml:"version"`
	Position string `toml:"position"`
}

type Prepend struct {
	ID      string   `toml:"id"`
	Image   string   `toml:"image"`
	Version string   `toml:"version"`
	Groups  []string `toml:"groups"`
}

// Generate builds the builder of a snapshot from the base builder and the overlay. Without a snapshot it builds the
// base builder with the overlay's fixed versions only.
func Generate(base Builder, overlay Overlay, snapshot *Snapshot) (Builder, error) {
	if err := validate(base, overlay); err != nil {
		return Builder{}, err
	}

	var versions map[string]string
	if snapshot != nil {
		versions = snapshot.Buildpacks
	}

	builder, err := apply(base, overlay, versions)
	if err != nil && snapshot != nil {
		return Builder{}, fmt.Errorf("snapshot %s: %w", snapshot.Name, err)
	}
	return builder, err
}

func validate(base Builder, overlay Overlay) error {
	baseIDs := map[string]bool{}
	for _, order := range base.Order {
		for _, entry := range order.Group {
			baseIDs[entry.ID] = true
		}
	}

	for _, arch := range overlay.Arches {
		if !slices.ContainsFunc(base.Targets, func(t Target) bool { return t.Arch == arch }) {
			return fmt.Errorf("arch %s isn't one of the base builder's targets", arch)
		}
	}

	known := maps.Clone(baseIDs)
	for _, stack := range overlay.Stacks {
		if stack.Image == "" {
			return fmt.Errorf("stack %s has no image", stack.ID)
		}
		if baseIDs[stack.ID] {
			if stack.Track == "" || stack.Version != "" || stack.Position != "" {
				return fmt.Errorf("stack %s is already in the base builder, it can only be listed to track its versions", stack.ID)
			}
			continue
		}
		if (stack.Track == "") == (stack.Version == "") {
			return fmt.Errorf("stack %s needs either track or version", stack.ID)
		}
		known[stack.ID] = true
	}

	for _, prepend := range overlay.Prepend {
		if baseIDs[prepend.ID] {
			return fmt.Errorf("prepended buildpack %s is already in the base builder, remove it from the overlay", prepend.ID)
		}
		for _, id := range prepend.Groups {
			if !known[id] {
				return fmt.Errorf("prepended buildpack %s targets group %s, which doesn't exist", prepend.ID, id)
			}
		}
	}

	return nil
}

// apply adds the overlay to the base builder. versions holds the versions of the tracked stacks; tracked stacks
// without one are left out, or keep their base version if the base builder has them.
func apply(base Builder, overlay Overlay, versions map[string]string) (Builder, error) {
	builder := base
	builder.Description = overlay.Description
	builder.Buildpacks = slices.Clone(base.Buildpacks)
	builder.Order = slices.Clone(base.Order)
	builder.Targets = slices.DeleteFunc(slices.Clone(base.Targets), func(t Target) bool {
		return len(overlay.Arches) > 0 && !slices.Contains(overlay.Arches, t.Arch)
	})

	firsts := 0
	for _, stack := range overlay.Stacks {
		version := stack.Version
		if stack.Track != "" {
			version = versions[stack.ID]
		}

		if slices.ContainsFunc(base.Order, func(o Order) bool { return o.contains(stack.ID) }) {
			if version != "" {
				if err := builder.setVersion(stack, version); err != nil {
					return Builder{}, err
				}
			}
			continue
		}
		if version == "" {
			continue
		}

		builder.Buildpacks = append(builder.Buildpacks, buildpack(stack.Image, version))
		group := Order{Group: []GroupEntry{{ID: stack.ID, Version: version}}}

		switch position, ref, _ := strings.Cut(stack.Position, ":"); position {
		case "first":
			builder.Order = slices.Insert(builder.Order, firsts, group)
			firsts++
		case "last", "":
			builder.Order = append(builder.Order, group)
		case "before", "after":
			i := slices.IndexFunc(builder.Order, func(o Order) bool { return o.contains(ref) })
			if i < 0 {
				return Builder{}, fmt.Errorf("stack %s: no order group contains %s", stack.ID, ref)
			}
			if position == "after" {
				i++
			}
			builder.Order = slices.Insert(builder.Order, i, group)
		default:
			return Builder{}, fmt.Errorf("stack %s: invalid position %q", stack.ID, stack.Position)
		}
	}

	for _, prepend := range overlay.Prepend {
		builder.Buildpacks = append(builder.Buildpacks, buildpack(prepend.Image, prepend.Version))
	}

	// Prepend in reverse so that the entries end up in the order they are listed in.
	for _, prepend := range slices.Backward(overlay.Prepend) {
		entry := GroupEntry{ID: prepend.ID, Version: prepend.Version, Optional: true}
		for i, order := range builder.Order {
			if len(prepend.Groups) == 0 || slices.ContainsFunc(prepend.Groups, order.contains) {
				builder.Order[i].Group = slices.Insert(slices.Clone(order.Group), 0, entry)
			}
		}
	}

	return builder, nil
}

// setVersion switches a stack of the base builder to another version.
func (b *Builder) setVersion(stack OverlayStack, version string) error {
	prefix := "docker://" + stack.Image + ":"
	i := slices.IndexFunc(b.Buildpacks, func(bp Buildpack) bool { return strings.HasPrefix(bp.URI, prefix) })
	if i < 0 {
		return fmt.Errorf("stack %s: the base builder has no buildpack with image %s", stack.ID, stack.Image)
	}
	b.Buildpacks[i] = buildpack(stack.Image, version)

	for i, order := range b.Order {
		if !order.contains(stack.ID) {
			continue
		}
		group := slices.Clone(order.Group)
		for j := range group {
			if group[j].ID == stack.ID {
				group[j].Version = version
			}
		}
		b.Order[i].Group = group
	}

	return nil
}

func (o Order) contains(id string) bool {
	return slices.ContainsFunc(o.Group, func(entry GroupEntry) bool { return entry.ID == id })
}

func buildpack(image, version string) Buildpack {
	return Buildpack{URI: fmt.Sprintf("docker://%s:%s", image, version), Version: version}
}

// Encode writes the builder in the same layout as the upstream builder.toml.
func Encode(b Builder) []byte {
	var buf bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&buf, format, args...) }

	w("description = %q\n", b.Description)
	w("\n[build]\n  image = %q\n", b.Build.Image)
	for _, bp := range b.Buildpacks {
		w("\n[[buildpacks]]\n  uri = %q\n  version = %q\n", bp.URI, bp.Version)
	}
	w("\n[lifecycle]\n  version = %q\n", b.Lifecycle.Version)
	for _, order := range b.Order {
		w("\n[[order]]\n")
		for _, entry := range order.Group {
			w("\n  [[order.group]]\n    id = %q\n    version = %q\n", entry.ID, entry.Version)
			if entry.Optional {
				w("    optional = true\n")
			}
		}
	}
	w("\n[run]\n")
	for _, image := range b.Run.Images {
		w("\n  [[run.images]]\n    image = %q\n", image.Image)
	}
	w("\n[stack]\n  id = %q\n", b.Stack.ID)
	for _, target := range b.Targets {
		w("\n[[targets]]\n  arch = %q\n  os = %q\n", target.Arch, target.OS)
	}

	return buf.Bytes()
}
