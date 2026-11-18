package depclosure

import (
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/slimtoolkit/slim/pkg/util/filter"
)

// fakeFS is a small in-memory filesystem used to verify the dependency
// closure without running a dynamic linker or building real binaries.
type fakeFS struct {
	files   map[string]string // path -> symlink target ("" for regular files)
	exists  map[string]bool
	lstatOK map[string]bool
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:   map[string]string{},
		exists:  map[string]bool{},
		lstatOK: map[string]bool{},
	}
}

func (f *fakeFS) file(p string) *fakeFS {
	f.files[p] = ""
	f.exists[p] = true
	f.lstatOK[p] = true
	return f
}

func (f *fakeFS) link(p, target string) *fakeFS {
	f.files[p] = target
	f.exists[p] = true
	f.lstatOK[p] = true
	return f
}

func (f *fakeFS) plugin() Options {
	resolve := func(filePath string) ([]string, error) {
		return nil, nil
	}

	return Options{
		FileExists: func(p string) bool { return f.exists[p] },
		Lstat: func(p string) (os.FileInfo, error) {
			if target, found := f.files[p]; found {
				if target != "" {
					return fakeInfo{p: p, mode: os.ModeSymlink}, nil
				}

				return fakeInfo{p: p, mode: 0}, nil
			}

			return nil, os.ErrNotExist
		},
		Readlink: func(p string) (string, error) {
			target, found := f.files[p]
			if !found {
				return "", os.ErrNotExist
			}

			return target, nil
		},
		Resolve:     resolve,
		SearchPaths: []string{"/lib", "/usr/lib"},
	}
}

type fakeInfo struct {
	p    string
	mode os.FileMode
}

func (f fakeInfo) Name() string       { return f.p }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() interface{}   { return nil }

func rules(t *testing.T, list ...filter.Rule) *filter.RuleSet {
	t.Helper()

	rs, err := filter.Compile(list, filter.Options{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	return rs
}

func exc(p string) filter.Rule { return filter.Rule{Pattern: p, Action: filter.Exclude} }
func inc(p string) filter.Rule { return filter.Rule{Pattern: p, Action: filter.Include} }

func TestClosureTransitive(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/lib/liba.so.1").
		file("/lib/libb.so.1").
		file("/lib/libc.so.6")

	opts := fs.plugin()
	opts.Rules = rules(t, exc("/var/log/**"))

	graph := map[string][]string{
		"/usr/bin/app":   {"/lib/liba.so.1"},
		"/lib/liba.so.1": {"/lib/libb.so.1", "libc.so.6"},
		"/lib/libb.so.1": nil,
	}

	opts.Resolve = func(p string) ([]string, error) { return graph[p], nil }

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	want := []string{"/usr/bin/app", "/lib/liba.so.1", "/lib/libb.so.1", "/lib/libc.so.6"}
	if !equalStrings(result.Paths, want) {
		t.Fatalf("paths=%v, want %v (warnings: %v)", result.Paths, want, result.Warnings)
	}

	if len(result.Excluded) != 0 || len(result.Missing) != 0 {
		t.Fatalf("unexpected exclusions/missing: %+v / %v", result.Excluded, result.Missing)
	}
}

func TestClosureExplicitExcludeWinsAndWarns(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/lib/liba.so.1").
		file("/opt/vendor/libv.so")

	opts := fs.plugin()
	opts.Rules = rules(t, exc("/opt/vendor/**"))
	opts.Resolve = func(p string) ([]string, error) {
		switch p {
		case "/usr/bin/app":
			return []string{"/lib/liba.so.1", "/opt/vendor/libv.so"}, nil
		default:
			return nil, nil
		}
	}

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	if !equalStrings(result.Paths, []string{"/usr/bin/app", "/lib/liba.so.1"}) {
		t.Fatalf("paths=%v", result.Paths)
	}

	if len(result.Excluded) != 1 || result.Excluded[0].Path != "/opt/vendor/libv.so" {
		t.Fatalf("expected the excluded dependency to be reported: %+v", result.Excluded)
	}

	if result.Excluded[0].Pattern != "/opt/vendor/**" {
		t.Fatalf("exclusion should name the winning rule: %+v", result.Excluded[0])
	}

	if len(result.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", result.Warnings)
	}

	if !contains(result.Warnings[0], "may fail to run") {
		t.Fatalf("warning must explain the consequence: %q", result.Warnings[0])
	}
}

func TestClosureMissingDependencyDoesNotAbort(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/lib/liba.so.1")

	opts := fs.plugin()
	opts.Rules = rules(t)
	opts.Resolve = func(p string) ([]string, error) {
		switch p {
		case "/usr/bin/app":
			return []string{"/lib/libmissing.so", "liba.so.1"}, nil
		default:
			return nil, nil
		}
	}

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	if !equalStrings(result.Paths, []string{"/usr/bin/app", "/lib/liba.so.1"}) {
		t.Fatalf("closure must continue past the missing dependency: %v", result.Paths)
	}

	if len(result.Missing) != 1 || result.Missing[0] != "/lib/libmissing.so" {
		t.Fatalf("missing dependency not reported: %v", result.Missing)
	}

	if len(result.Warnings) != 1 || !contains(result.Warnings[0], "could not be resolved") {
		t.Fatalf("missing warning not reported: %v", result.Warnings)
	}
}

func TestClosureResolverErrorDoesNotAbort(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/lib/liba.so.1").
		file("/lib/broken.so")

	opts := fs.plugin()
	opts.Rules = rules(t)
	opts.Resolve = func(p string) ([]string, error) {
		switch p {
		case "/usr/bin/app":
			return []string{"/lib/broken.so", "/lib/liba.so.1"}, nil
		case "/lib/broken.so":
			return nil, errors.New("resolver exploded")
		default:
			return nil, nil
		}
	}

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	if !sliceContains(result.Paths, "/lib/liba.so.1") || !sliceContains(result.Paths, "/lib/broken.so") || !sliceContains(result.Paths, "/usr/bin/app") {
		t.Fatalf("resolver failure on one object must not abort the others: %v", result.Paths)
	}

	found := false
	for _, m := range result.Missing {
		if contains(m, "/lib/broken.so") {
			found = true
		}
	}

	if !found {
		t.Fatalf("resolver failure not reported as missing: %v", result.Missing)
	}
}

func TestClosureSymlinkAndTarget(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		link("/lib/libz.so.1", "libz.so.1.2.13").
		file("/lib/libz.so.1.2.13")

	opts := fs.plugin()
	opts.Rules = rules(t)
	opts.Resolve = func(p string) ([]string, error) {
		if p == "/usr/bin/app" {
			return []string{"/lib/libz.so.1"}, nil
		}

		return nil, nil
	}

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	want := []string{"/usr/bin/app", "/lib/libz.so.1", "/lib/libz.so.1.2.13"}
	if !equalStrings(result.Paths, want) {
		t.Fatalf("paths=%v, want %v", result.Paths, want)
	}
}

func TestClosureSymlinkCycleTerminates(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		link("/lib/a.so", "b.so").
		link("/lib/b.so", "a.so")

	opts := fs.plugin()
	opts.Rules = rules(t)
	opts.Resolve = func(p string) ([]string, error) {
		if p == "/usr/bin/app" {
			return []string{"/lib/a.so"}, nil
		}

		return nil, nil
	}

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	if !equalStrings(result.Paths, []string{"/usr/bin/app", "/lib/a.so", "/lib/b.so"}) {
		t.Fatalf("paths=%v", result.Paths)
	}
}

func TestClosureLibrarySearchPath(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/opt/custom/lib/libe.so").
		file("/usr/lib/libd.so")

	opts := fs.plugin()
	opts.Rules = rules(t)
	opts.SearchPaths = []string{"/opt/custom/lib", "/usr/lib"}
	opts.Resolve = func(p string) ([]string, error) {
		if p == "/usr/bin/app" {
			return []string{"libe.so", "libnotfound.so", "libd.so"}, nil
		}

		return nil, nil
	}

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	if !equalStrings(result.Paths, []string{"/usr/bin/app", "/opt/custom/lib/libe.so", "/usr/lib/libd.so"}) {
		t.Fatalf("library search path not applied: %v", result.Paths)
	}

	if len(result.Missing) != 1 || result.Missing[0] != "libnotfound.so" {
		t.Fatalf("unresolvable library must be reported: %v", result.Missing)
	}
}

func TestClosureNeededByDirectory(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/usr/lib/plugins/libp.so").
		file("/lib/libq.so")

	opts := fs.plugin()
	opts.Rules = rules(t)
	opts.Resolve = func(p string) ([]string, error) {
		switch p {
		case "/usr/bin/app":
			return []string{"/lib/libq.so"}, nil
		case "/usr/lib/plugins/libp.so":
			return []string{"libq.so"}, nil
		default:
			return nil, nil
		}
	}

	// Relative name resolved next to the object first.
	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)
	if !equalStrings(result.Paths, []string{"/usr/bin/app", "/lib/libq.so"}) {
		t.Fatalf("paths=%v", result.Paths)
	}
}

func TestClosureExplicitExecutableKeptByDefault(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/lib/liba.so")

	opts := fs.plugin()
	opts.Rules = rules(t, exc("/usr/bin/app"))
	opts.Resolve = func(p string) ([]string, error) { return []string{"/lib/liba.so"}, nil }

	// The executable was requested explicitly, so only its dependencies are
	// filtered by default.
	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)
	if !equalStrings(result.Paths, []string{"/usr/bin/app", "/lib/liba.so"}) {
		t.Fatalf("paths=%v", result.Paths)
	}

	if len(result.Excluded) != 0 {
		t.Fatalf("unexpected exclusions: %+v", result.Excluded)
	}

	// FilterRoots makes the roots subject to the same rules.
	opts.FilterRoots = true
	result = Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)
	if len(result.Paths) != 0 {
		t.Fatalf("an explicitly excluded executable must not pull dependencies: %v", result.Paths)
	}

	if len(result.Excluded) != 1 || result.Excluded[0].Path != "/usr/bin/app" {
		t.Fatalf("up-front exclusion not reported: %+v", result.Excluded)
	}
}

func TestClosureIncludeKeepsExcludedDependency(t *testing.T) {
	fs := newFakeFS().
		file("/usr/bin/app").
		file("/opt/vendor/libv.so")

	opts := fs.plugin()
	opts.Rules = rules(t, exc("/opt/vendor/**"), inc("/opt/vendor/libv.so"))
	opts.Resolve = func(p string) ([]string, error) {
		if p == "/usr/bin/app" {
			return []string{"/opt/vendor/libv.so"}, nil
		}

		return nil, nil
	}

	result := Compute(Hints{Executables: []string{"/usr/bin/app"}}, opts)

	if !equalStrings(result.Paths, []string{"/usr/bin/app", "/opt/vendor/libv.so"}) {
		t.Fatalf("more specific include must win during completion: %v (excluded: %+v)", result.Paths, result.Excluded)
	}
}

func TestDefaultSearchPaths(t *testing.T) {
	paths := DefaultSearchPaths("/a/lib:/b/lib:/a/lib")
	want := []string{"/a/lib", "/b/lib", "/lib", "/lib64", "/usr/lib", "/usr/lib64", "/usr/local/lib", "/usr/local/lib64"}
	if !equalStrings(paths, want) {
		t.Fatalf("paths=%v, want %v", paths, want)
	}
}

func TestComputeNilRules(t *testing.T) {
	result := Compute(Hints{Executables: []string{"/bin/x"}}, Options{
		Resolve: func(string) ([]string, error) { return nil, nil },
	})

	if len(result.Warnings) == 0 {
		t.Fatal("expected a warning for a nil rule set")
	}
}

func sliceContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}

	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	x := append([]string{}, a...)
	y := append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}

	return true
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
