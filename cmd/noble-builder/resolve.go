package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// Lock is snapshots.json: every published snapshot, oldest first.
type Lock struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// Snapshot is one published builder version: the upstream builder release it is based on, the versions of the
// tracked stacks as they were on Date, and the language versions those provide on the base stack.
type Snapshot struct {
	Version    string              `json:"version"`
	Date       string              `json:"date"`
	Base       string              `json:"base"`
	Buildpacks map[string]string   `json:"buildpacks"`
	Languages  map[string][]string `json:"languages"`
}

// State is the latest release of every tracked stack at a point in time.
type State struct {
	Date       time.Time
	Buildpacks map[string]string
	Languages  map[string][]string
}

// StackRelease is a release of a tracked composite buildpack and the language versions it supports on the base stack.
type StackRelease struct {
	Version   string
	Published time.Time
	Languages []string
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

type buildpackTOML struct {
	Stacks  []bpStack  `toml:"stacks"`
	Targets []bpTarget `toml:"targets"`
	Order   []struct {
		Group []GroupEntry `toml:"group"`
	} `toml:"order"`
	Metadata struct {
		Dependencies []dependency `toml:"dependencies"`
	} `toml:"metadata"`
}

type bpStack struct {
	ID string `toml:"id"`
}

type bpTarget struct {
	OS   string `toml:"os"`
	Arch string `toml:"arch"`
}

type dependency struct {
	ID      string   `toml:"id"`
	Version string   `toml:"version"`
	Stacks  []string `toml:"stacks"`
	Arch    string   `toml:"arch"`
}

type Resolver struct {
	StackID string
	Arches  []string
	Verbose bool

	client   *http.Client
	cacheDir string
	mu       sync.Mutex
	inflight map[string]*sync.Once
	results  map[string]fetchResult
}

type fetchResult struct {
	bp  *buildpackTOML
	err error
}

var errNotFound = errors.New("not found")

func NewResolver() (*Resolver, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}

	return &Resolver{
		client:   &http.Client{Timeout: 30 * time.Second},
		cacheDir: filepath.Join(cacheDir, "paketo-noble-builder"),
		inflight: map[string]*sync.Once{},
		results:  map[string]fetchResult{},
	}, nil
}

// StackReleases returns the releases of a composite buildpack that work on the base stack, newest first. It walks
// back from the newest release and stops at the first one whose tracked component has no build for the base stack at
// all, since base stack support only ever starts at some release and older ones can't have it.
func (r *Resolver) StackReleases(repo, track string) ([]StackRelease, error) {
	releases, err := r.releases(repo)
	if err != nil {
		return nil, err
	}

	type result struct {
		languages []string
		reason    string
		err       error
	}
	results := make([]result, len(releases))

	// Resolve in batches so the walk back can stop early without fetching the whole history.
	const batch = 16
	var supported []StackRelease
	for start := 0; start < len(releases); start += batch {
		end := min(start+batch, len(releases))

		var wg sync.WaitGroup
		for i := start; i < end; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				languages, reason, err := r.supportedLanguages(repo, releases[i].TagName, track)
				results[i] = result{languages, reason, err}
			}()
		}
		wg.Wait()

		for i := start; i < end; i++ {
			res, release := results[i], releases[i]
			if res.err != nil {
				return nil, fmt.Errorf("%s %s: %w", repo, release.TagName, res.err)
			}
			if res.languages == nil {
				r.logf("  %s %s: stop, %s", repo, release.TagName, res.reason)
				return supported, nil
			}
			if res.reason != "" {
				r.logf("  %s %s: skipped, %s", repo, release.TagName, res.reason)
				continue
			}
			supported = append(supported, StackRelease{
				Version:   strings.TrimPrefix(release.TagName, "v"),
				Published: release.PublishedAt,
				Languages: res.languages,
			})
		}
	}

	return supported, nil
}

// supportedLanguages returns the versions of the tracked dependency that the composite release provides on the base
// stack. It returns nil languages when the tracked component has none at all, and a reason when the release can't be
// used because another component doesn't support the base stack.
func (r *Resolver) supportedLanguages(repo, tag, track string) ([]string, string, error) {
	composite, err := r.buildpackTOML(repo, tag)
	if errors.Is(err, errNotFound) {
		return nil, "no buildpack.toml", nil
	}
	if err != nil {
		return nil, "", err
	}

	components := map[string]string{}
	for _, order := range composite.Order {
		for _, entry := range order.Group {
			components[entry.ID] = entry.Version
		}
	}

	var languages []string
	var tracked bool
	var reason string
	for _, id := range slices.Sorted(maps.Keys(components)) {
		component, err := r.buildpackTOML(id, "v"+components[id])
		if errors.Is(err, errNotFound) {
			reason = fmt.Sprintf("can't find %s %s", id, components[id])
			continue
		}
		if err != nil {
			return nil, "", err
		}

		providesTrack := slices.ContainsFunc(component.Metadata.Dependencies, func(dep dependency) bool { return dep.ID == track })
		if problem := r.incompatibility(component); problem != "" {
			if providesTrack {
				// base stack support only ever starts at some release, so older ones can't have it either
				return nil, fmt.Sprintf("%s %s %s", id, components[id], problem), nil
			}
			if reason == "" {
				reason = fmt.Sprintf("%s %s %s", id, components[id], problem)
			}
			continue
		}

		// Group the dependencies by id, and check that every stack specific one has a build for every arch.
		byID := map[string][]string{}
		for _, dep := range component.Metadata.Dependencies {
			if !r.matchesStack(dep.Stacks) {
				byID[dep.ID] = append(byID[dep.ID], "")
				continue
			}
			if dep.Arch != "" {
				byID[dep.ID] = append(byID[dep.ID], dep.Version+"/"+dep.Arch)
				continue
			}
			// packit ignores the arch of dependencies, so ones without an arch (e.g. gems) serve every arch
			for _, arch := range r.Arches {
				byID[dep.ID] = append(byID[dep.ID], dep.Version+"/"+arch)
			}
		}

		for depID, entries := range byID {
			versions := r.versionsOnAllArches(entries)
			if depID == track {
				tracked = true
				languages = append(languages, versions...)
			} else if len(versions) == 0 && reason == "" {
				reason = fmt.Sprintf("%s %s has no %s build of %s", id, components[id], r.StackID, depID)
			}
		}
	}

	if !tracked {
		return nil, fmt.Sprintf("no component provides %s", track), nil
	}
	if len(languages) == 0 {
		return nil, fmt.Sprintf("no %s build of %s", r.StackID, track), nil
	}

	sortVersions(languages)
	return slices.Compact(languages), reason, nil
}

// incompatibility says why a component buildpack can't go into a builder for the base stack and arches, if it
// can't. pack refuses buildpacks whose stacks don't include the builder's, and buildpacks without targets predate
// multi arch images and are amd64 only.
func (r *Resolver) incompatibility(bp *buildpackTOML) string {
	if len(bp.Stacks) > 0 && !slices.ContainsFunc(bp.Stacks, func(s bpStack) bool {
		return s.ID == "*" || s.ID == r.StackID
	}) {
		return "doesn't support " + r.StackID
	}

	for _, arch := range r.Arches {
		supported := slices.ContainsFunc(bp.Targets, func(t bpTarget) bool {
			return t.Arch == arch && (t.OS == "" || t.OS == "linux")
		})
		if !supported && (len(bp.Targets) > 0 || arch != "amd64") {
			return "has no linux/" + arch + " target"
		}
	}

	return ""
}

func (r *Resolver) matchesStack(stacks []string) bool {
	return len(stacks) == 0 || slices.Contains(stacks, "*") || slices.Contains(stacks, r.StackID)
}

// versionsOnAllArches takes "version/arch" entries and returns the versions available on every required arch.
func (r *Resolver) versionsOnAllArches(entries []string) []string {
	arches := map[string]map[string]bool{}
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		version, arch, _ := strings.Cut(entry, "/")
		if arches[version] == nil {
			arches[version] = map[string]bool{}
		}
		arches[version][arch] = true
	}

	var versions []string
	for version, available := range arches {
		if !slices.ContainsFunc(r.Arches, func(arch string) bool { return !available[arch] }) {
			versions = append(versions, version)
		}
	}
	return versions
}

// releases lists the published releases of a repo, newest first. The list is cached for an hour, since listing
// costs API calls and unauthenticated ones are limited to 60 an hour.
func (r *Resolver) releases(repo string) ([]ghRelease, error) {
	cachePath := filepath.Join(r.cacheDir, repo, "releases.json")
	if info, err := os.Stat(cachePath); err == nil && time.Since(info.ModTime()) < time.Hour {
		content, err := os.ReadFile(cachePath)
		if err != nil {
			return nil, err
		}
		var releases []ghRelease
		if err := json.Unmarshal(content, &releases); err != nil {
			return nil, err
		}
		return releases, nil
	}

	const perPage = 100
	var releases []ghRelease
	for page := 1; ; page++ {
		body, err := r.api(fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d&page=%d", repo, perPage, page))
		if err != nil {
			return nil, err
		}

		var batch []ghRelease
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		for _, release := range batch {
			if !release.Draft && !release.Prerelease {
				releases = append(releases, release)
			}
		}
		if len(batch) < perPage {
			break
		}
	}

	sort.Slice(releases, func(i, j int) bool { return releases[i].PublishedAt.After(releases[j].PublishedAt) })

	content, err := json.Marshal(releases)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(cachePath, content, 0o644); err != nil {
		return nil, err
	}

	return releases, nil
}

// buildpackTOML fetches a buildpack.toml at a tag. Tags don't change, so it is cached on disk, and concurrent
// requests for the same file share one fetch.
func (r *Resolver) buildpackTOML(repo, tag string) (*buildpackTOML, error) {
	key := repo + "@" + tag

	r.mu.Lock()
	once, ok := r.inflight[key]
	if !ok {
		once = &sync.Once{}
		r.inflight[key] = once
	}
	r.mu.Unlock()

	once.Do(func() {
		bp, err := r.fetchBuildpackTOML(repo, tag)
		r.mu.Lock()
		r.results[key] = fetchResult{bp, err}
		r.mu.Unlock()
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.results[key].bp, r.results[key].err
}

func (r *Resolver) fetchBuildpackTOML(repo, tag string) (*buildpackTOML, error) {
	content, err := r.FetchFile(repo, tag, "buildpack.toml")
	if err != nil {
		return nil, err
	}

	var bp buildpackTOML
	if _, err := toml.Decode(string(content), &bp); err != nil {
		return nil, fmt.Errorf("decoding %s@%s buildpack.toml: %w", repo, tag, err)
	}
	return &bp, nil
}

// FetchFile fetches a file of a repo at a tag. Tags don't change, so files are cached on disk, including the fact
// that one doesn't exist.
func (r *Resolver) FetchFile(repo, tag, path string) ([]byte, error) {
	cachePath := filepath.Join(r.cacheDir, repo, tag, path)
	missingPath := cachePath + ".missing"

	if _, err := os.Stat(missingPath); err == nil {
		return nil, errNotFound
	}

	content, err := os.ReadFile(cachePath)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return content, err
	}

	content, err = r.download(fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", repo, tag, path))
	if errors.Is(err, errNotFound) {
		_ = os.MkdirAll(filepath.Dir(missingPath), 0o755)
		_ = os.WriteFile(missingPath, nil, 0o644)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return nil, err
	}
	return content, os.WriteFile(cachePath, content, 0o644)
}

// LatestRelease returns the tag of the latest release of a repo.
func (r *Resolver) LatestRelease(repo string) (string, error) {
	body, err := r.api(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo))
	if err != nil {
		return "", err
	}

	var release ghRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return "", err
	}
	return release.TagName, nil
}

// api GETs a GitHub API URL, authenticated with GITHUB_TOKEN when it is set.
func (r *Resolver) api(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s (set GITHUB_TOKEN to avoid rate limits)", url, resp.Status, body)
	}
	return body, nil
}

func (r *Resolver) download(url string) ([]byte, error) {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		resp, err := r.client.Get(url)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode == http.StatusNotFound:
			return nil, errNotFound
		case resp.StatusCode != http.StatusOK:
			lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
		default:
			return body, nil
		}
	}
	return nil, lastErr
}

func (r *Resolver) logf(format string, args ...any) {
	if r.Verbose {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// Timeline returns the state after each release of the tracked stacks, oldest first.
func Timeline(stacks map[string][]StackRelease, tracks map[string]string) []State {
	type event struct {
		stack   string
		release StackRelease
	}
	var events []event
	for stack, releases := range stacks {
		for _, release := range releases {
			events = append(events, event{stack, release})
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].release.Published.Equal(events[j].release.Published) {
			return events[i].release.Published.Before(events[j].release.Published)
		}
		return events[i].stack < events[j].stack
	})

	var states []State
	current := map[string]StackRelease{}
	for _, e := range events {
		current = maps.Clone(current)
		current[e.stack] = e.release

		state := State{Date: e.release.Published, Buildpacks: map[string]string{}, Languages: map[string][]string{}}
		for stack, release := range current {
			state.Buildpacks[stack] = release.Version
			state.Languages[tracks[stack]] = release.Languages
		}
		states = append(states, state)
	}
	return states
}

// PickStates returns the indexes of the fewest states that offer every language version that isn't covered yet,
// oldest first.
//
// Each language version is available for one or more contiguous stretches of the timeline. Picking the end of the
// earliest ending stretch that isn't offered by a picked state yet, until all are, gives the fewest states (interval
// stabbing). Stretches that are still open end at the last state, so it is picked whenever a version that isn't
// covered yet is available now.
func PickStates(states []State, covered map[string]bool) []int {
	type stretch struct{ start, end int }
	var stretches []stretch
	open := map[string]int{}
	for i, state := range states {
		available := map[string]bool{}
		for language, versions := range state.Languages {
			for _, version := range versions {
				if key := language + "@" + version; !covered[key] {
					available[key] = true
				}
			}
		}
		for key, start := range open {
			if !available[key] {
				stretches = append(stretches, stretch{start, i - 1})
				delete(open, key)
			}
		}
		for key := range available {
			if _, ok := open[key]; !ok {
				open[key] = i
			}
		}
	}
	for _, start := range open {
		stretches = append(stretches, stretch{start, len(states) - 1})
	}

	sort.Slice(stretches, func(i, j int) bool { return stretches[i].end < stretches[j].end })
	var picked []int
	last := -1
	for _, s := range stretches {
		if s.start > last {
			last = s.end
			picked = append(picked, last)
		}
	}
	return picked
}

// NextVersion returns the version after the newest snapshot: the next patch of the series, or its first one.
func NextVersion(series string, snapshots []Snapshot) string {
	if len(snapshots) == 0 {
		return series + ".0"
	}
	patch, ok := strings.CutPrefix(snapshots[len(snapshots)-1].Version, series+".")
	if !ok {
		return series + ".0"
	}
	var n int
	fmt.Sscanf(patch, "%d", &n)
	return fmt.Sprintf("%s.%d", series, n+1)
}

func sortVersions(versions []string) {
	slices.SortFunc(versions, compareVersions)
}

// compareVersions compares dotted versions numerically, falling back to string comparison for non numeric parts.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		var an, bn int
		_, aErr := fmt.Sscanf(as[i], "%d", &an)
		_, bErr := fmt.Sscanf(bs[i], "%d", &bn)
		if aErr == nil && bErr == nil && an != bn {
			return an - bn
		}
		if aErr != nil || bErr != nil {
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return len(as) - len(bs)
}
