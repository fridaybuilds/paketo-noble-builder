package main

import (
	"strings"
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
	suite("Snapshots", testSnapshots)
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
		snapshot = Snapshot{Name: "2026.02.01", Buildpacks: map[string]string{"ruby": "2.2.0", "nodejs": "2.2.0"}}
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
			Base: "v0.0.203",
			Snapshots: []Snapshot{
				{Name: "2026.01.01", Buildpacks: map[string]string{"ruby": "2.1.0"}, Languages: map[string][]string{"ruby": {"3.3.0", "3.4.0"}}},
				{Name: "2026.02.01", Buildpacks: map[string]string{"ruby": "2.2.0", "nodejs": "2.2.0"}, Languages: map[string][]string{"ruby": {"3.4.0", "3.4.1"}, "node": {"22.1.0"}}},
			},
		}
	)

	it("lists every language version with the newest snapshot that has it", func() {
		docs := string(SupportedVersions(overlay, lock))

		Expect(docs).To(ContainSubstring("## Ruby\n\n| Version | Image tag |\n| --- | --- |\n| 3.4.1 | `2026.02.01` |\n| 3.4.0 | `2026.02.01` |\n| 3.3.0 | `2026.01.01` |\n"))
		Expect(docs).To(ContainSubstring("## Node.js\n\n| Version | Image tag |\n| --- | --- |\n| 22.1.0 | `2026.02.01` |\n"))
		Expect(strings.Index(docs, "## Ruby")).To(BeNumerically("<", strings.Index(docs, "## Node.js")))
	})

	context("Notes", func() {
		var builder Builder

		it.Before(func() {
			var err error
			builder, err = Generate(testBase(), overlay, &lock.Snapshots[1])
			Expect(err).NotTo(HaveOccurred())
		})

		it("lists the language versions, the changes and the buildpacks", func() {
			notes, err := Notes(overlay, lock, "2026.02.01", builder, nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(string(notes)).To(ContainSubstring("--builder docker.io/fridaybuilds/paketo-noble-builder:2026.02.01"))
			Expect(string(notes)).To(ContainSubstring("| Ruby | 3.4.0, 3.4.1 |\n| Node.js | 22.1.0 |\n"))
			Expect(string(notes)).To(ContainSubstring("### Changes since 2026.01.01\n\n- **Ruby**: added 3.4.1; dropped 3.3.0\n- **Node.js**: added 22.1.0\n"))
			Expect(string(notes)).To(ContainSubstring("| apt (optional) | 0.3.0 |\n| vips (optional) | 0.0.4 |\n| ruby | 2.2.0 |\n"))
			Expect(string(notes)).NotTo(ContainSubstring("## Revisions"))
		})

		it("lists the revisions newest first", func() {
			revisions := []Revision{
				{Revision: 1, Date: "2026-02-01", Base: "v0.0.200", BuildImage: "docker.io/build:0.0.130", Lifecycle: "0.21.20", Digest: "sha256:aaa"},
				{Revision: 2, Date: "2026-02-08", Base: "v0.0.203", BuildImage: "docker.io/build:0.0.138", Lifecycle: "0.21.22", Digest: "sha256:bbb"},
			}

			notes, err := Notes(overlay, lock, "2026.02.01", builder, revisions)
			Expect(err).NotTo(HaveOccurred())

			Expect(string(notes)).To(ContainSubstring(
				"| `2026.02.01-r2` | 2026-02-08 | [v0.0.203](https://github.com/paketo-buildpacks/ubuntu-noble-builder/releases/tag/v0.0.203) | 0.0.138 | 0.21.22 | `sha256:bbb` |\n" +
					"| `2026.02.01-r1` | 2026-02-01 |"))
		})

		it("says so for the first snapshot", func() {
			notes, err := Notes(overlay, lock, "2026.01.01", builder, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(notes)).To(ContainSubstring("This is the first snapshot."))
		})

		it("fails for an unknown snapshot", func() {
			_, err := Notes(overlay, lock, "2025.01.01", builder, nil)
			Expect(err).To(MatchError("no snapshot 2025.01.01"))
		})
	})
}

func testSnapshots(t *testing.T, context spec.G, it spec.S) {
	var Expect = NewWithT(t).Expect

	day := func(d int) time.Time { return time.Date(2026, 1, d, 12, 0, 0, 0, time.UTC) }
	tracks := map[string]string{"ruby": "ruby", "nodejs": "node"}

	it("picks the fewest points that cover every language version", func() {
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

		snapshots := Snapshots(stacks, tracks)

		// day 2 covers ruby 3.3.0 before 1.2.0 drops it, and with it ruby 3.3.1 and node 20.0.0. day 6 is the
		// current state, which covers the rest.
		Expect(snapshots).To(Equal([]Snapshot{
			{Name: "2026.01.02", Buildpacks: map[string]string{"ruby": "1.1.0", "nodejs": "5.0.0"}, Languages: map[string][]string{"ruby": {"3.3.0", "3.3.1"}, "node": {"20.0.0"}}},
			{Name: "2026.01.06", Buildpacks: map[string]string{"ruby": "1.4.0", "nodejs": "5.1.0"}, Languages: map[string][]string{"ruby": {"3.3.2", "3.4.0"}, "node": {"22.0.0"}}},
		}))
	})

	it("keeps earlier snapshots when new releases come out", func() {
		stacks := map[string][]StackRelease{
			"ruby": {
				{Version: "1.0.0", Published: day(1), Languages: []string{"3.3.0"}},
				{Version: "1.1.0", Published: day(2), Languages: []string{"3.3.1"}},
			},
		}
		before := Snapshots(stacks, tracks)

		stacks["ruby"] = append(stacks["ruby"], StackRelease{Version: "1.2.0", Published: day(3), Languages: []string{"3.3.1"}})
		after := Snapshots(stacks, tracks)

		Expect(after[0]).To(Equal(before[0]))
		Expect(after[len(after)-1].Buildpacks["ruby"]).To(Equal("1.2.0"))
	})

	it("counts a version that comes back as a separate stretch", func() {
		stacks := map[string][]StackRelease{
			"ruby": {
				{Version: "1.0.0", Published: day(1), Languages: []string{"3.3.0"}},
				{Version: "1.1.0", Published: day(2), Languages: []string{"3.4.0"}},
				{Version: "1.2.0", Published: day(3), Languages: []string{"3.3.0"}},
			},
		}

		var names []string
		for _, snapshot := range Snapshots(stacks, tracks) {
			names = append(names, snapshot.Name)
		}
		Expect(names).To(Equal([]string{"2026.01.01", "2026.01.02", "2026.01.03"}))
	})

	it("suffixes snapshots picked on the same day", func() {
		morning, evening := day(1), day(1).Add(6*time.Hour)
		stacks := map[string][]StackRelease{
			"ruby": {
				{Version: "1.0.0", Published: morning, Languages: []string{"3.3.0"}},
				{Version: "1.1.0", Published: evening, Languages: []string{"3.4.0"}},
			},
		}

		snapshots := Snapshots(stacks, tracks)
		Expect(snapshots[0].Name).To(Equal("2026.01.01"))
		Expect(snapshots[1].Name).To(Equal("2026.01.01-2"))
	})

	it("returns nothing without releases", func() {
		Expect(Snapshots(nil, tracks)).To(BeEmpty())
	})

	context("MergeSnapshots", func() {
		snapshot := func(name, version string) Snapshot {
			return Snapshot{Name: name, Buildpacks: map[string]string{"ruby": version}}
		}

		it("keeps previous snapshots and adds the newly picked ones in date order", func() {
			previous := []Snapshot{snapshot("2026.01.01", "1.0.0"), snapshot("2026.01.05", "1.1.0")}
			picked := []Snapshot{snapshot("2026.01.03", "1.0.5"), snapshot("2026.01.09", "1.2.0")}

			var names []string
			for _, s := range MergeSnapshots(previous, picked) {
				names = append(names, s.Name)
			}
			Expect(names).To(Equal([]string{"2026.01.01", "2026.01.03", "2026.01.09"}))
		})

		it("keeps the previous newest snapshot when it's picked again", func() {
			previous := []Snapshot{snapshot("2026.01.01", "1.0.0"), snapshot("2026.01.05", "1.1.0")}
			picked := []Snapshot{snapshot("2026.01.05", "1.1.0"), snapshot("2026.01.09", "1.2.0")}

			Expect(MergeSnapshots(previous, picked)).To(Equal([]Snapshot{
				snapshot("2026.01.01", "1.0.0"), snapshot("2026.01.05", "1.1.0"), snapshot("2026.01.09", "1.2.0"),
			}))
		})

		it("prefers previous snapshots over picked ones with the same name", func() {
			previous := []Snapshot{snapshot("2026.01.01", "1.0.0"), snapshot("2026.01.05", "1.1.0")}
			picked := []Snapshot{snapshot("2026.01.01", "0.9.0"), snapshot("2026.01.09", "1.2.0")}

			Expect(MergeSnapshots(previous, picked)[0]).To(Equal(snapshot("2026.01.01", "1.0.0")))
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
