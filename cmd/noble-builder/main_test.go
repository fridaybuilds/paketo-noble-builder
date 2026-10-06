package main

import (
	"testing"
	"time"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	. "github.com/onsi/gomega"
)

func TestNobleBuilder(t *testing.T) {
	suite := spec.New("noble-builder", spec.Report(report.Terminal{}))
	suite("Generate", testGenerate)
	suite("Docs", testDocs)
	suite("Versions", testVersions)
	suite("Resolver", testResolver)
	suite.Run(t)
}

func groupIDs(b Builder) [][]string {
	var ids [][]string
	for _, order := range b.Order {
		var group []string
		for _, entry := range order.Group {
			group = append(group, entry.ID)
		}
		ids = append(ids, group)
	}
	return ids
}

func testBase() Builder {
	return Builder{
		Description: "upstream",
		Buildpacks: []Buildpack{
			{URI: "docker://docker.io/ruby:1.0.0", Version: "1.0.0"},
			{URI: "docker://docker.io/java:1.0.0", Version: "1.0.0"},
			{URI: "docker://docker.io/nodejs:2.0.0", Version: "2.0.0"},
		},
		Order: []Order{
			{Group: []GroupEntry{{ID: "ruby", Version: "1.0.0"}}},
			{Group: []GroupEntry{{ID: "java", Version: "1.0.0"}}},
			{Group: []GroupEntry{{ID: "nodejs", Version: "2.0.0"}}},
		},
		Targets: []Target{{Arch: "amd64", OS: "linux"}, {Arch: "arm64", OS: "linux"}},
	}
}

func testOverlay() Overlay {
	return Overlay{
		Base:        "paketo-buildpacks/ubuntu-noble-builder",
		Image:       "docker.io/fridaybuilds/paketo-noble-builder",
		Series:      "0.1",
		Description: "fridaybuilds",
		Stacks: []OverlayStack{
			{ID: "ruby", Image: "docker.io/ruby", Track: "ruby"},
			{ID: "go", Image: "docker.io/go", Version: "3.0.0", Position: "after:java"},
			{ID: "nodejs", Image: "docker.io/nodejs", Track: "node"},
		},
		Prepend: []Prepend{
			{ID: "apt", Image: "docker.io/apt", Version: "0.3.0"},
			{ID: "vips", Image: "docker.io/vips", Version: "0.0.4", Groups: []string{"ruby", "nodejs"}},
		},
	}
}

func testGenerate(t *testing.T, context spec.G, it spec.S) {
	var (
		Expect = NewWithT(t).Expect

		base     Builder
		overlay  Overlay
		snapshot Snapshot
	)

	it.Before(func() {
		base = testBase()
		overlay = testOverlay()
		snapshot = Snapshot{Version: "0.1.1", Buildpacks: map[string]string{"ruby": "2.2.0", "nodejs": "2.2.0"}}
	})

	it("switches the tracked stacks to the snapshot's versions", func() {
		builder, err := Generate(base, overlay, &snapshot)
		Expect(err).NotTo(HaveOccurred())

		Expect(builder.Description).To(Equal("fridaybuilds"))
		Expect(builder.Buildpacks).To(ConsistOf(
			Buildpack{URI: "docker://docker.io/ruby:2.2.0", Version: "2.2.0"},
			Buildpack{URI: "docker://docker.io/java:1.0.0", Version: "1.0.0"},
			Buildpack{URI: "docker://docker.io/nodejs:2.2.0", Version: "2.2.0"},
			Buildpack{URI: "docker://docker.io/go:3.0.0", Version: "3.0.0"},
			Buildpack{URI: "docker://docker.io/apt:0.3.0", Version: "0.3.0"},
			Buildpack{URI: "docker://docker.io/vips:0.0.4", Version: "0.0.4"},
		))
		Expect(builder.Order[0].Group).To(ContainElement(GroupEntry{ID: "ruby", Version: "2.2.0"}))
		Expect(builder.Order[3].Group).To(ContainElement(GroupEntry{ID: "nodejs", Version: "2.2.0"}))
	})

	it("places stacks and prepends optional buildpacks in the listed order", func() {
		builder, err := Generate(base, overlay, &snapshot)
		Expect(err).NotTo(HaveOccurred())

		Expect(groupIDs(builder)).To(Equal([][]string{
			{"apt", "vips", "ruby"},
			{"apt", "java"},
			{"apt", "go"},
			{"apt", "vips", "nodejs"},
		}))
		Expect(builder.Order[0].Group[0]).To(Equal(GroupEntry{ID: "apt", Version: "0.3.0", Optional: true}))
		Expect(builder.Order[0].Group[1].Optional).To(BeTrue())
	})

	it("keeps the base version of tracked base stacks that the snapshot doesn't have", func() {
		snapshot.Buildpacks = map[string]string{"nodejs": "2.1.0"}

		builder, err := Generate(base, overlay, &snapshot)
		Expect(err).NotTo(HaveOccurred())
		Expect(builder.Order[0].Group).To(ContainElement(GroupEntry{ID: "ruby", Version: "1.0.0"}))
	})

	it("leaves out the arches that aren't listed", func() {
		overlay.Arches = []string{"amd64"}

		builder, err := Generate(base, overlay, &snapshot)
		Expect(err).NotTo(HaveOccurred())
		Expect(builder.Targets).To(Equal([]Target{{Arch: "amd64", OS: "linux"}}))
		Expect(base.Targets).To(HaveLen(2))
	})

	it("doesn't modify the base builder", func() {
		_, err := Generate(base, overlay, &snapshot)
		Expect(err).NotTo(HaveOccurred())

		Expect(base.Buildpacks[0].Version).To(Equal("1.0.0"))
		Expect(groupIDs(base)).To(Equal([][]string{{"ruby"}, {"java"}, {"nodejs"}}))
		Expect(base.Order[2].Group[0].Version).To(Equal("2.0.0"))
	})

	it("encodes the builder in the upstream layout", func() {
		builder, err := Generate(base, overlay, &snapshot)
		Expect(err).NotTo(HaveOccurred())

		Expect(string(Encode(builder))).To(ContainSubstring(
			"[[order]]\n\n  [[order.group]]\n    id = \"apt\"\n    version = \"0.3.0\"\n    optional = true\n"))
	})

	context("failure cases", func() {
		it("fails when a base stack is listed without track", func() {
			overlay.Stacks[2] = OverlayStack{ID: "nodejs", Image: "docker.io/nodejs", Version: "2.0.0"}
			_, err := Generate(base, overlay, &snapshot)
			Expect(err).To(MatchError(ContainSubstring("it can only be listed to track its versions")))
		})

		it("fails when a stack has both track and version", func() {
			overlay.Stacks[1].Track = "go"
			_, err := Generate(base, overlay, &snapshot)
			Expect(err).To(MatchError(ContainSubstring("needs either track or version")))
		})

		it("fails when a prepend targets an unknown group", func() {
			overlay.Prepend[1].Groups = []string{"python"}
			_, err := Generate(base, overlay, &snapshot)
			Expect(err).To(MatchError(ContainSubstring("targets group python, which doesn't exist")))
		})

		it("fails when a position references an unknown group", func() {
			overlay.Stacks[1].Position = "before:python"
			_, err := Generate(base, overlay, &snapshot)
			Expect(err).To(MatchError(ContainSubstring("no order group contains python")))
		})

		it("fails when a tracked base stack's image isn't in the base builder", func() {
			overlay.Stacks[2].Image = "docker.io/other"
			_, err := Generate(base, overlay, &snapshot)
			Expect(err).To(MatchError(ContainSubstring("has no buildpack with image docker.io/other")))
		})

		it("fails on an arch the base builder doesn't have", func() {
			overlay.Arches = []string{"riscv64"}
			_, err := Generate(base, overlay, &snapshot)
			Expect(err).To(MatchError(ContainSubstring("arch riscv64 isn't one of the base builder's targets")))
		})
	})
}

func testDocs(t *testing.T, context spec.G, it spec.S) {
	var (
		Expect = NewWithT(t).Expect

		overlay = testOverlay()
		lock    = Lock{
			Snapshots: []Snapshot{
				{Version: "0.1.0", Date: "2026-01-01", Base: "v0.0.200", Buildpacks: map[string]string{"ruby": "2.1.0"}, Languages: map[string][]string{"ruby": {"3.3.0", "3.4.0"}}},
				{Version: "0.1.1", Date: "2026-02-01", Base: "v0.0.203", Buildpacks: map[string]string{"ruby": "2.2.0", "nodejs": "2.2.0"}, Languages: map[string][]string{"ruby": {"3.4.0", "3.4.1"}, "node": {"22.1.0"}}},
			},
		}
	)

	context("Notes", func() {
		var builder, previous Builder

		it.Before(func() {
			var err error
			previous, err = Generate(testBase(), overlay, &lock.Snapshots[0])
			Expect(err).NotTo(HaveOccurred())
			builder, err = Generate(testBase(), overlay, &lock.Snapshots[1])
			Expect(err).NotTo(HaveOccurred())
		})

		it("describes the version", func() {
			notes := string(Notes(overlay, lock, 1, builder, &previous))

			Expect(notes).To(ContainSubstring("buildpacks as of 2026-02-01, based on the Paketo noble builder\n[v0.0.203](https://github.com/paketo-buildpacks/ubuntu-noble-builder/releases/tag/v0.0.203)"))
			Expect(notes).To(ContainSubstring("--builder docker.io/fridaybuilds/paketo-noble-builder:0.1.1"))
			Expect(notes).To(ContainSubstring("| Ruby | 3.4.0, 3.4.1 |\n| Node.js | 22.1.0 |\n"))
			Expect(notes).To(ContainSubstring("| apt (optional) | 0.3.0 |\n| vips (optional) | 0.0.4 |\n| ruby | 2.2.0 |\n"))
		})

		it("lists the language and builder changes since the previous version", func() {
			previous.Build.Image = "docker.io/build:0.0.130"
			builder.Build.Image = "docker.io/build:0.0.138"

			notes := string(Notes(overlay, lock, 1, builder, &previous))

			Expect(notes).To(ContainSubstring("## Changes since 0.1.0\n\n" +
				"- **Ruby**: added 3.4.1; dropped 3.3.0\n" +
				"- **Node.js**: added 22.1.0\n" +
				"- `ruby`: 2.1.0 → 2.2.0\n" +
				"- `nodejs`: 2.0.0 → 2.2.0\n" +
				"- Build image: 0.0.130 → 0.0.138\n"))
		})

		it("says when nothing changed", func() {
			notes := string(Notes(overlay, lock, 1, builder, &builder))

			Expect(notes).To(ContainSubstring("- **Ruby**"))
			Expect(notes).NotTo(ContainSubstring("`ruby`: "))
		})

		it("says so for the first version", func() {
			notes := string(Notes(overlay, lock, 0, previous, nil))
			Expect(notes).To(ContainSubstring("This is the first version."))
		})
	})
}

func testVersions(t *testing.T, context spec.G, it spec.S) {
	var Expect = NewWithT(t).Expect

	day := func(d int) time.Time { return time.Date(2026, 1, d, 12, 0, 0, 0, time.UTC) }
	tracks := map[string]string{"ruby": "ruby", "nodejs": "node"}

	// ruby 3.3.0 is available on days 1-2, 3.3.1 on days 2-4, 3.3.2 from day 3 and 3.4.0 from day 4.
	// node 20.0.0 is available on days 1-4, 22.0.0 from day 5.
	stacks := map[string][]StackRelease{
		"ruby": {
			{Version: "1.0.0", Published: day(1), Languages: []string{"3.3.0"}},
			{Version: "1.1.0", Published: day(2), Languages: []string{"3.3.0", "3.3.1"}},
			{Version: "1.2.0", Published: day(3), Languages: []string{"3.3.1", "3.3.2"}},
			{Version: "1.3.0", Published: day(4), Languages: []string{"3.3.1", "3.3.2", "3.4.0"}},
			{Version: "1.4.0", Published: day(6), Languages: []string{"3.3.2", "3.4.0"}},
		},
		"nodejs": {
			{Version: "5.0.0", Published: day(1), Languages: []string{"20.0.0"}},
			{Version: "5.1.0", Published: day(5), Languages: []string{"22.0.0"}},
		},
	}

	context("Timeline", func() {
		it("returns the latest release of every stack after each release", func() {
			states := Timeline(stacks, tracks)

			Expect(states).To(HaveLen(7))
			Expect(states[1]).To(Equal(State{
				Date:       day(1),
				Buildpacks: map[string]string{"ruby": "1.0.0", "nodejs": "5.0.0"},
				Languages:  map[string][]string{"ruby": {"3.3.0"}, "node": {"20.0.0"}},
			}))
			Expect(states[6].Buildpacks).To(Equal(map[string]string{"ruby": "1.4.0", "nodejs": "5.1.0"}))
		})

		it("returns nothing without releases", func() {
			Expect(Timeline(nil, tracks)).To(BeEmpty())
		})
	})

	context("PickStates", func() {
		it("picks the fewest states that offer every language version", func() {
			states := Timeline(stacks, tracks)

			// day 2 offers ruby 3.3.0 before it is dropped, and with it 3.3.1 and node 20.0.0. The last state offers
			// the rest.
			picked := PickStates(states, map[string]bool{})
			Expect(picked).To(Equal([]int{2, 6}))
			Expect(states[2].Buildpacks).To(Equal(map[string]string{"ruby": "1.1.0", "nodejs": "5.0.0"}))
		})

		it("only picks states for the versions that aren't covered yet", func() {
			states := Timeline(stacks, tracks)

			covered := map[string]bool{"ruby@3.3.0": true, "ruby@3.3.1": true, "node@20.0.0": true}
			Expect(PickStates(states, covered)).To(Equal([]int{6}))

			for _, state := range states {
				for language, versions := range state.Languages {
					for _, version := range versions {
						covered[language+"@"+version] = true
					}
				}
			}
			Expect(PickStates(states, covered)).To(BeEmpty())
		})

		it("counts a version that comes back as a separate stretch", func() {
			states := Timeline(map[string][]StackRelease{
				"ruby": {
					{Version: "1.0.0", Published: day(1), Languages: []string{"3.3.0"}},
					{Version: "1.1.0", Published: day(2), Languages: []string{"3.4.0"}},
					{Version: "1.2.0", Published: day(3), Languages: []string{"3.3.0"}},
				},
			}, tracks)

			Expect(PickStates(states, map[string]bool{})).To(Equal([]int{0, 1, 2}))
		})
	})

	context("NextVersion", func() {
		it("starts the series at .0", func() {
			Expect(NextVersion("0.1", nil)).To(Equal("0.1.0"))
		})

		it("increments the last number", func() {
			Expect(NextVersion("0.1", []Snapshot{{Version: "0.1.0"}, {Version: "0.1.9"}})).To(Equal("0.1.10"))
		})

		it("starts a new series at .0", func() {
			Expect(NextVersion("0.2", []Snapshot{{Version: "0.1.12"}})).To(Equal("0.2.0"))
		})
	})
}

func testResolver(t *testing.T, context spec.G, it spec.S) {
	var (
		Expect   = NewWithT(t).Expect
		resolver = &Resolver{StackID: "io.buildpacks.stacks.noble", Arches: []string{"amd64", "arm64"}}
	)

	context("incompatibility", func() {
		it("accepts buildpacks for the base stack or any stack with every target", func() {
			targets := []bpTarget{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}
			Expect(resolver.incompatibility(&buildpackTOML{Stacks: []bpStack{{ID: "*"}}, Targets: targets})).To(BeEmpty())
			Expect(resolver.incompatibility(&buildpackTOML{Stacks: []bpStack{{ID: "io.buildpacks.stacks.noble"}}, Targets: targets})).To(BeEmpty())
			Expect(resolver.incompatibility(&buildpackTOML{Targets: targets})).To(BeEmpty())
		})

		it("rejects buildpacks for other stacks", func() {
			Expect(resolver.incompatibility(&buildpackTOML{Stacks: []bpStack{{ID: "io.buildpacks.stacks.jammy"}}})).
				To(Equal("doesn't support io.buildpacks.stacks.noble"))
		})

		it("rejects buildpacks missing a target, treating no targets as amd64 only", func() {
			Expect(resolver.incompatibility(&buildpackTOML{Targets: []bpTarget{{OS: "linux", Arch: "amd64"}}})).
				To(Equal("has no linux/arm64 target"))
			Expect(resolver.incompatibility(&buildpackTOML{Stacks: []bpStack{{ID: "*"}}})).
				To(Equal("has no linux/arm64 target"))

			amd64Only := &Resolver{StackID: "io.buildpacks.stacks.noble", Arches: []string{"amd64"}}
			Expect(amd64Only.incompatibility(&buildpackTOML{Stacks: []bpStack{{ID: "*"}}})).To(BeEmpty())
		})
	})

	context("versionsOnAllArches", func() {
		it("only returns versions available on every arch", func() {
			versions := resolver.versionsOnAllArches([]string{"3.3.0/amd64", "3.3.0/arm64", "3.4.0/amd64", ""})
			Expect(versions).To(Equal([]string{"3.3.0"}))
		})
	})

	context("compareVersions", func() {
		it("compares numerically", func() {
			versions := []string{"3.10.1", "3.9.2", "3.10.0", "3.9"}
			sortVersions(versions)
			Expect(versions).To(Equal([]string{"3.9", "3.9.2", "3.10.0", "3.10.1"}))
		})
	})
}
