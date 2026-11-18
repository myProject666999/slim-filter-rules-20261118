package filter

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/bmatcuk/doublestar/v3"
)

// ---------------------------------------------------------------------------
// Random rule-set generation for property tests.
//
// The generators are deliberately broad: literal paths, glob segments,
// doublestar, trailing-slash directory rules, and both actions. The
// properties below must hold for every generated rule set, not just for the
// curated table.
// ---------------------------------------------------------------------------

type generator struct {
	rnd   *rand.Rand
	dirs  []string
	files []string
}

func newGenerator(seed int64) *generator {
	g := &generator{rnd: rand.New(rand.NewSource(seed))}

	depth := 4
	segs := []string{"app", "var", "log", "data", "lib", "tmp", "opt", "usr"}

	var build func(prefix string, d int)
	build = func(prefix string, d int) {
		g.dirs = append(g.dirs, prefix)
		if d == 0 {
			return
		}

		for _, s := range segs {
			build(prefix+"/"+s, d-1)
		}
	}

	build("", depth-1)
	g.dirs = append(g.dirs, "/")

	for _, d := range g.dirs {
		for _, f := range []string{"main", "x.log", "test.txt", "keep.log", "data.bin", ".env"} {
			g.files = append(g.files, d+"/"+f)
		}
	}

	return g
}

func (g *generator) path() (string, bool) {
	if g.rnd.Intn(2) == 0 && len(g.files) > 0 {
		return g.files[g.rnd.Intn(len(g.files))], false
	}

	// Only emit directories that are in the known tree (plus "/"), so the
	// expectations below can reason about containment.
	return g.dirs[g.rnd.Intn(len(g.dirs))], true
}

func (g *generator) pattern() string {
	segs := []string{"app", "var", "log", "data", "lib", "tmp", "opt", "usr",
		"*", "**", "*.log", "keep.log", "main", "[a-z]*", "test.txt", "?", "data*"}

	n := 1 + g.rnd.Intn(3)
	pattern := ""
	for i := 0; i < n; i++ {
		pattern += "/" + segs[g.rnd.Intn(len(segs))]
	}

	if g.rnd.Intn(8) == 0 {
		pattern += "/"
	}

	return pattern
}

func (g *generator) rules(n int) []Rule {
	rules := make([]Rule, 0, n)
	for i := 0; i < n; i++ {
		action := Exclude
		if g.rnd.Intn(2) == 0 {
			action = Include
		}

		rules = append(rules, Rule{Pattern: g.pattern(), Action: action})
	}

	return rules
}

type pathResult struct {
	path  string
	isDir bool
	keep  bool
}

func decideAll(t *testing.T, rules []Rule, opts Options, paths []pathResult) []pathResult {
	t.Helper()

	rs, err := Compile(rules, opts)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	out := make([]pathResult, len(paths))
	for i, p := range paths {
		out[i] = pathResult{path: p.path, isDir: p.isDir, keep: rs.Decide(p.path, p.isDir).Keep}
	}

	return out
}

func randomPaths(g *generator, n int) []pathResult {
	paths := make([]pathResult, 0, n)
	for i := 0; i < n; i++ {
		p, isDir := g.path()
		paths = append(paths, pathResult{path: p, isDir: isDir})
	}

	return paths
}

// Property: permuting the rules must not change any decision.
func TestPropertyRuleOrderIrrelevant(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		g := newGenerator(seed)
		rules := g.rules(1 + g.rnd.Intn(8))
		paths := randomPaths(g, 120)

		base := decideAll(t, rules, Options{}, paths)

		for shuffle := 0; shuffle < 5; shuffle++ {
			perm := make([]Rule, len(rules))
			copy(perm, rules)
			g.rnd.Shuffle(len(perm), func(i, j int) {
				perm[i], perm[j] = perm[j], perm[i]
			})

			got := decideAll(t, perm, Options{}, paths)
			for i := range base {
				if base[i].keep != got[i].keep {
					t.Fatalf("seed=%d shuffle=%d path=%q keep=%v with original order, %v after permutation\nrules: %v",
						seed, shuffle, base[i].path, base[i].keep, got[i].keep, rules)
				}
			}
		}
	}
}

// Property: adding an include never removes a path that was kept before.
func TestPropertyAddingIncludeNeverShrinks(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		g := newGenerator(seed + 1000)
		rules := g.rules(1 + g.rnd.Intn(8))
		paths := randomPaths(g, 120)

		base := decideAll(t, rules, Options{}, paths)

		extraInclude := Rule{Pattern: g.pattern(), Action: Include, Source: "property:new-include"}
		extended := append(append([]Rule{}, rules...), extraInclude)
		got := decideAll(t, extended, Options{}, paths)

		for i := range base {
			if base[i].keep && !got[i].keep {
				t.Fatalf("seed=%d path=%q was kept before adding include %q but removed after\nrules: %v",
					seed, base[i].path, extraInclude.Pattern, rules)
			}
		}
	}
}

// Property: adding an exclude may only remove paths where the new exclude is
// the winning rule under the documented specificity rules.
func TestPropertyAddingExcludeRemovesOnlyItsWinners(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		g := newGenerator(seed + 2000)
		rules := g.rules(1 + g.rnd.Intn(8))
		paths := randomPaths(g, 120)

		base := decideAll(t, rules, Options{}, paths)

		extraExclude := Rule{Pattern: g.pattern(), Action: Exclude, Source: "property:new-exclude"}
		extended := append(append([]Rule{}, rules...), extraExclude)

		rs, err := Compile(extended, Options{})
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}

		got := decideAll(t, extended, Options{}, paths)
		for i := range base {
			if !base[i].keep || got[i].keep {
				continue
			}

			d := rs.Decide(base[i].path, base[i].isDir)
			if d.Matched == nil || d.Matched.Source != extraExclude.Source || d.Matched.Action != Exclude {
				t.Fatalf("seed=%d path=%q was removed but the new exclude %q did not win; winner=%v\nrules: %v\nexplanation: %s",
					seed, base[i].path, extraExclude.Pattern, d.Matched, rules, d.Explanation)
			}

			// Sanity: nothing may be removed unless the exclude actually
			// matches the path (directly or through an ancestor).
			if !matchedSource(d, extraExclude.Source) {
				t.Fatalf("seed=%d path=%q removed by a rule that does not match it", seed, base[i].path)
			}
		}
	}
}

func matchedSource(d Decision, source string) bool {
	for _, m := range d.Matches {
		if m.Source == source {
			return true
		}
	}

	return false
}

// Property: a path protected by a more specific include is never removed by a
// shallower exclude, whatever the rule order.
func TestPropertySpecificIncludeProtects(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		g := newGenerator(seed + 3000)

		dir := g.dirs[g.rnd.Intn(len(g.dirs))]
		if dir == "/" {
			dir = ""
		}

		include := Rule{
			Pattern: dir + "/data/keep.log",
			Action:  Include,
		}

		shallow := []string{"/**", "/app/**", "/var/**", "/*/**", "/app/*/**"}
		exclude := Rule{
			Pattern: shallow[g.rnd.Intn(len(shallow))],
			Action:  Exclude,
		}

		extra := g.rules(3)
		rules := append([]Rule{exclude, include}, extra...)

		rs, err := Compile(rules, Options{})
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}

		d := rs.Decide(include.Pattern, false)
		if !d.Keep {
			// Only acceptable when another rule with an even deeper literal
			// anchor excludes this exact file.
			if d.Matched == nil || d.Matched.Action != Exclude || d.Matched.Pattern != include.Pattern {
				t.Fatalf("seed=%d specific include %q lost to %v\nrules: %v\nexplanation: %s",
					seed, include.Pattern, d.Matched, rules, d.Explanation)
			}
		}
	}
}

// Property: for exclude-only rule sets, legacy mode never removes a path that
// the current semantics keeps, and both modes agree whenever the excluding
// pattern matched the path directly. The only intended difference is that the
// current semantics also applies a matched directory rule to its contents (the
// previously undefined boundary), which is listed in the behavior-change
// notes.
func TestPropertyLegacyIsSubsetForExcludeOnly(t *testing.T) {
	for seed := int64(0); seed < 80; seed++ {
		g := newGenerator(seed + 4000)

		n := 1 + g.rnd.Intn(10)
		rules := make([]Rule, 0, n)
		for i := 0; i < n; i++ {
			rules = append(rules, Rule{Pattern: g.pattern(), Action: Exclude})
		}

		paths := randomPaths(g, 200)

		current := decideAll(t, rules, Options{}, paths)
		legacy := decideAll(t, rules, Options{Legacy: true}, paths)

		rs, err := Compile(rules, Options{})
		if err != nil {
			t.Fatal(err)
		}

		for i := range current {
			if current[i].keep && !legacy[i].keep {
				t.Fatalf("seed=%d path=%q current=keep legacy=exclude; legacy must be a subset\nrules: %v",
					seed, current[i].path, rules)
			}

			directOnly := false
			for _, r := range rules {
				if directMatch(r.Pattern, current[i].path) {
					directOnly = true
					break
				}
			}

			if directOnly && current[i].keep != legacy[i].keep {
				t.Fatalf("seed=%d path=%q direct-pattern decision differs: current=%v legacy=%v\nrules: %v",
					seed, current[i].path, current[i].keep, legacy[i].keep, rules)
			}

			_ = rs
		}
	}
}

func directMatch(pattern, p string) bool {
	ok, err := doublestar.Match(pattern, p)
	return err == nil && ok
}

// Property: decisions are deterministic and independent of prior queries.
func TestPropertyDeterministicAcrossQueries(t *testing.T) {
	g := newGenerator(5000)
	rules := g.rules(12)
	paths := randomPaths(g, 300)

	first := decideAll(t, rules, Options{}, paths)

	g2 := newGenerator(5000 + 99)
	reordered := randomPaths(g2, len(paths))
	_ = decideAll(t, rules, Options{}, reordered)

	second := decideAll(t, rules, Options{}, paths)
	for i := range first {
		if first[i].keep != second[i].keep {
			t.Fatalf("path=%q changed result between runs: %v vs %v", first[i].path, first[i].keep, second[i].keep)
		}
	}
}

// Property: explanation and match list are stable for a given decision.
func TestPropertyExplanationStable(t *testing.T) {
	g := newGenerator(6000)
	rules := g.rules(6)

	rs, err := Compile(rules, Options{})
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}
	for _, p := range randomPaths(g, 80) {
		d := rs.Decide(p.path, p.isDir)
		key := fmt.Sprintf("%q/%v", p.path, p.isDir)
		if prev, found := seen[key]; found && prev != d.Explanation {
			t.Fatalf("unstable explanation for %s:\n%s\n%s", key, prev, d.Explanation)
		}

		seen[key] = d.Explanation

		// The reported match list must be ordered most specific first.
		for i := 1; i < len(d.Matches); i++ {
			if compareMatch(d.Matches[i-1], d.Matches[i]) < 0 {
				t.Fatalf("match list not ordered by precedence for %s: %v", key, d.Matches)
			}
		}
	}
}

// The compiled rule set must be independent from the order in which rules are
// supplied at the structural level as well (indexes, not just decisions).
func TestPropertyCompiledRuleSetStable(t *testing.T) {
	g := newGenerator(7000)
	rules := g.rules(20)

	rs1, err := Compile(rules, Options{})
	if err != nil {
		t.Fatal(err)
	}

	perm := append([]Rule{}, rules...)
	g.rnd.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })

	rs2, err := Compile(perm, Options{})
	if err != nil {
		t.Fatal(err)
	}

	normalized := func(rs *RuleSet) []string {
		var out []string
		for _, r := range rs.Rules() {
			out = append(out, r.Action.String()+" "+r.Pattern)
		}

		sort.Strings(out)

		return out
	}

	a, b := normalized(rs1), normalized(rs2)
	if len(a) != len(b) {
		t.Fatalf("rule count changed: %v vs %v", a, b)
	}

	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("normalized rule sets differ: %v vs %v", a, b)
		}
	}
}
