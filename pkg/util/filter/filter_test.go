package filter

import (
	"fmt"
	"testing"
)

// The table below is the normative specification of the matching semantics.
// Each case states its expected outcome and the reason independently of the
// implementation, so a behavior change breaks the test instead of silently
// redefining the rules.
type tableCase struct {
	name     string
	rules    []Rule
	path     string
	isDir    bool
	opts     Options
	wantKeep bool
	reason   string
}

func inc(p string) Rule { return Rule{Pattern: p, Action: Include} }
func exc(p string) Rule { return Rule{Pattern: p, Action: Exclude} }

func TestDecisionTable(t *testing.T) {
	cases := []tableCase{
		// ---- basics -------------------------------------------------------
		{
			name:     "noRulesKeepsEverything",
			path:     "/app/main",
			wantKeep: true,
			reason:   "with no rules the filter must not remove anything",
		},
		{
			name:     "singleExactExclude",
			rules:    []Rule{exc("/etc/secret")},
			path:     "/etc/secret",
			wantKeep: false,
			reason:   "an exact exclude removes exactly that path",
		},
		{
			name:     "singleExactInclude",
			rules:    []Rule{inc("/app/main")},
			path:     "/app/main",
			wantKeep: true,
			reason:   "an exact include keeps the path",
		},
		{
			name:     "unmatchedPathKeptByDefault",
			rules:    []Rule{exc("/etc/secret")},
			path:     "/app/main",
			wantKeep: true,
			reason:   "paths that no rule matches are kept (filtering, not allow-listing)",
		},
		{
			name:     "unmatchedPathDroppedWithDropUnmatched",
			rules:    []Rule{inc("/app/main")},
			path:     "/app/other",
			opts:     Options{DropUnmatched: true},
			wantKeep: false,
			reason:   "DropUnmatched turns the rule set into an allow-list",
		},

		// ---- directory rules apply to descendants -------------------------
		{
			name:     "excludeDirExcludesContents",
			rules:    []Rule{exc("/var/log")},
			path:     "/var/log/syslog",
			wantKeep: false,
			reason:   "a rule matching a directory applies to everything below it",
		},
		{
			name:     "excludeDirDoesNotMatchSiblingPrefix",
			rules:    []Rule{exc("/var/log")},
			path:     "/var/logrotate.conf",
			wantKeep: true,
			reason:   "matching is per path segment; /var/log must not match /var/logrotate.conf",
		},
		{
			name:     "includeDirKeepsContents",
			rules:    []Rule{inc("/app")},
			path:     "/app/lib/x.so",
			wantKeep: true,
			reason:   "an include of a directory keeps its contents",
		},

		// ---- the two motivating configurations ----------------------------
		{
			name:     "excludeLogDirKeepNeededFile",
			rules:    []Rule{exc("/var/log/**"), inc("/var/log/app/keep.log")},
			path:     "/var/log/app/keep.log",
			wantKeep: true,
			reason:   "the file-specific include is more specific than the directory exclude",
		},
		{
			name:     "excludeLogDirRemovesOtherFiles",
			rules:    []Rule{exc("/var/log/**"), inc("/var/log/app/keep.log")},
			path:     "/var/log/app/rotated.log",
			wantKeep: false,
			reason:   "only the explicitly included file survives the directory exclude",
		},
		{
			name:     "workdirExcludeDirUnderDirRule",
			rules:    []Rule{exc("/app/**"), inc("/app")},
			path:     "/app/main",
			wantKeep: false,
			reason:   "an include of an ancestor directory is less specific than an exclude of the subtree",
		},
		{
			name:     "keepAppDirExcludeTests",
			rules:    []Rule{inc("/app"), exc("/app/testdata/**")},
			path:     "/app/testdata/golden.txt",
			wantKeep: false,
			reason:   "the deeper exclude wins over the app directory include",
		},
		{
			name:     "keepAppDirExcludeDocs",
			rules:    []Rule{inc("/app"), exc("/app/docs")},
			path:     "/app/docs/index.md",
			wantKeep: false,
			reason:   "an exclude of a subdirectory applies to its contents",
		},
		{
			name:     "keepAppDirKeepOtherContent",
			rules:    []Rule{inc("/app"), exc("/app/testdata/**")},
			path:     "/app/bin/server",
			wantKeep: true,
			reason:   "the app include still protects everything not covered by the deeper exclude",
		},

		// ---- specificity tie-breaks ---------------------------------------
		{
			name:     "sameRuleIncludeWins",
			rules:    []Rule{exc("/app/data"), inc("/app/data")},
			path:     "/app/data",
			wantKeep: true,
			reason:   "at equal specificity include wins, to reduce accidental deletion",
		},
		{
			name:     "sameRuleIncludeWinsReversedOrder",
			rules:    []Rule{inc("/app/data"), exc("/app/data")},
			path:     "/app/data",
			wantKeep: true,
			reason:   "rule order must not change the outcome",
		},
		{
			name:     "exactBeatsGlob",
			rules:    []Rule{inc("/app/*.log"), exc("/app/debug.log")},
			path:     "/app/debug.log",
			wantKeep: false,
			reason:   "an exact path is more specific than a wildcard in the same directory",
		},
		{
			name:     "deeperLiteralBeatsShallowGlob",
			rules:    []Rule{exc("/app/*/*.txt"), inc("/app/data/x.txt")},
			path:     "/app/data/x.txt",
			wantKeep: true,
			reason:   "more literal path segments make the include more specific",
		},
		{
			name:     "moreLiteralsBeatsDoublestar",
			rules:    []Rule{inc("/**/*.so"), exc("/usr/lib/libz.so")},
			path:     "/usr/lib/libz.so",
			wantKeep: false,
			reason:   "a ** rule is less specific than a rule with a literal anchor",
		},
		{
			name:     "directMatchBeatsAncestorAtSameSpecificity",
			rules:    []Rule{exc("/app"), inc("/app/*")},
			path:     "/app",
			isDir:    true,
			wantKeep: false,
			reason:   "when specificity ties, a rule matching the path itself wins over an ancestor match",
		},

		// ---- wildcard semantics -------------------------------------------
		{
			name:     "starDoesNotCrossSlash",
			rules:    []Rule{exc("/var/*.log")},
			path:     "/var/log/x.log",
			wantKeep: true,
			reason:   "single star must not match across directory separators (note: an ancestor directory match would still apply here if /var/log matched)",
		},
		{
			name:     "starMatchesOnlyOwnSegment",
			rules:    []Rule{exc("/var/*.log")},
			path:     "/var/x.log",
			wantKeep: false,
			reason:   "single star matches a whole path segment",
		},
		{
			name:     "matchedAncestorDirAppliesToContents",
			rules:    []Rule{exc("/var/*")},
			path:     "/var/log/syslog",
			wantKeep: false,
			reason:   "/var/* matches the directory /var/log, and a directory rule covers its contents",
		},
		{
			name:     "doublestarCrossesSlash",
			rules:    []Rule{exc("/var/**")},
			path:     "/var/log/syslog",
			wantKeep: false,
			reason:   "double star matches across directory separators",
		},
		{
			name:     "doublestarMatchesZeroSegments",
			rules:    []Rule{exc("/var/**/x.log")},
			path:     "/var/x.log",
			wantKeep: false,
			reason:   "/**/ matches zero or more directories, matching common shell expectations",
		},
		{
			name:     "doublestarMatchesDeepSegments",
			rules:    []Rule{exc("/var/**/x.log")},
			path:     "/var/a/b/c/x.log",
			wantKeep: false,
			reason:   "/**/ matches any depth",
		},
		{
			name:     "questionMarkMatchesOneChar",
			rules:    []Rule{exc("/app/file?.txt")},
			path:     "/app/file1.txt",
			wantKeep: false,
			reason:   "? matches exactly one character",
		},
		{
			name:     "questionMarkDoesNotMatchSlash",
			rules:    []Rule{exc("/app/x?y")},
			path:     "/app/x/y",
			wantKeep: true,
			reason:   "? must not match a directory separator",
		},
		{
			name:     "questionMarkDoesNotMatchZero",
			rules:    []Rule{exc("/app/file?.txt")},
			path:     "/app/file.txt",
			wantKeep: true,
			reason:   "? requires exactly one character",
		},
		{
			name:     "charSetMatches",
			rules:    []Rule{exc("/app/log[123]")},
			path:     "/app/log2",
			wantKeep: false,
			reason:   "character sets select one of the listed characters",
		},
		{
			name:     "charSetRangeMatches",
			rules:    []Rule{exc("/app/log[a-c]")},
			path:     "/app/logb",
			wantKeep: false,
			reason:   "character set ranges are supported",
		},
		{
			name:     "charSetCaretNegation",
			rules:    []Rule{exc("/app/log[^0-9]")},
			path:     "/app/logx",
			wantKeep: false,
			reason:   "[^...] negates the class",
		},
		{
			name:     "charSetBangNegation",
			rules:    []Rule{exc("/app/log[!0-9]")},
			path:     "/app/logx",
			wantKeep: false,
			reason:   "[!...] is accepted as the alternate negation syntax",
		},
		{
			name:     "charSetNoMatchForExcludedChar",
			rules:    []Rule{exc("/app/log[123]")},
			path:     "/app/log9",
			wantKeep: true,
			reason:   "a character outside the set does not match",
		},
		{
			name:     "alternationMatches",
			rules:    []Rule{exc("/app/{log,tmp}/**")},
			path:     "/app/tmp/x",
			wantKeep: false,
			reason:   "brace alternation selects one of the alternatives",
		},
		{
			name:     "escapedMetaIsLiteral",
			rules:    []Rule{exc("/app/a\\*b")},
			path:     "/app/a*b",
			wantKeep: false,
			reason:   "an escaped meta character matches literally",
		},
		{
			name:     "escapedMetaIsNotWildcard",
			rules:    []Rule{exc("/app/a\\*b")},
			path:     "/app/axb",
			wantKeep: true,
			reason:   "an escaped star must not behave as a wildcard",
		},
		{
			name:     "escapedDirOnlyLiteral",
			rules:    []Rule{exc("/app/d\\*ir/")},
			path:     "/app/d*ir/x",
			wantKeep: false,
			reason:   "an escaped literal directory-only rule still covers the directory contents",
		},

		// ---- dot files ----------------------------------------------------
		{
			name:     "dotFileMatchesStar",
			rules:    []Rule{exc("/app/*")},
			path:     "/app/.env",
			wantKeep: false,
			reason:   "dot files participate in wildcard matching (pre-existing behavior)",
		},
		{
			name:     "dotDirMatchesDoublestar",
			rules:    []Rule{exc("/app/**")},
			path:     "/app/.config/x",
			wantKeep: false,
			reason:   "dot directories are not treated specially",
		},

		// ---- trailing slash -----------------------------------------------
		{
			name:     "trailingSlashMatchesDirectory",
			rules:    []Rule{exc("/app/cache/")},
			path:     "/app/cache",
			isDir:    true,
			wantKeep: false,
			reason:   "a trailing slash marks a directory-only rule",
		},
		{
			name:     "trailingSlashDoesNotMatchFile",
			rules:    []Rule{exc("/app/cache/")},
			path:     "/app/cache",
			isDir:    false,
			wantKeep: true,
			reason:   "a directory-only rule must not match a regular file of the same name",
		},
		{
			name:     "trailingSlashCoversContents",
			rules:    []Rule{exc("/app/cache/")},
			path:     "/app/cache/x",
			wantKeep: false,
			reason:   "a directory-only rule still applies to the contents of the directory",
		},

		// ---- relative paths -----------------------------------------------
		{
			name:     "relativePatternUsesContainerRoot",
			rules:    []Rule{exc("var/log")},
			path:     "/var/log/x",
			wantKeep: false,
			reason:   "relative patterns are resolved against the container root, not the process working directory",
		},
		{
			name:     "relativePathUsesContainerRoot",
			rules:    []Rule{exc("/var/log")},
			path:     "var/log/x",
			wantKeep: false,
			reason:   "relative paths are interpreted relative to the container root",
		},
		{
			name:     "relativeBaseOption",
			rules:    []Rule{exc("data/**")},
			path:     "/app/data/x",
			opts:     Options{Base: "/app"},
			wantKeep: false,
			reason:   "the Base option relocates relative patterns",
		},
		{
			name:     "cleanNormalizesDotSegments",
			rules:    []Rule{exc("/app/./data/../data/x")},
			path:     "/app/data/x",
			wantKeep: false,
			reason:   "paths and patterns are normalized before matching",
		},
		{
			name:     "excludeRootSubtree",
			rules:    []Rule{exc("/**"), inc("/app/main")},
			path:     "/app/main",
			wantKeep: true,
			reason:   "the deeper include survives a whole-tree exclude",
		},

		// ---- subtree anchors with wildcards -------------------------------
		{
			name:     "globSubtreeAnchorCoversAnchorDirItself",
			rules:    []Rule{exc("/opt/*/lib/**")},
			path:     "/opt/foo/lib",
			isDir:    true,
			wantKeep: false,
			reason:   "a trailing /** covers the anchor directory itself, even when the anchor has wildcards",
		},
		{
			name:     "globSubtreeAnchorDoesNotCoverParent",
			rules:    []Rule{exc("/opt/*/lib/**")},
			path:     "/opt/foo",
			isDir:    true,
			wantKeep: true,
			reason:   "the anchor's parents are not part of the subtree",
		},

		// ---- legacy mode ---------------------------------------------------
		{
			name:     "legacyExcludeWinsOverInclude",
			rules:    []Rule{exc("/app/data"), inc("/app/data")},
			path:     "/app/data",
			opts:     Options{Legacy: true},
			wantKeep: false,
			reason:   "legacy mode keeps the old 'any exclude wins' behavior",
		},
		{
			name:     "legacyKeepsUnmatched",
			rules:    []Rule{inc("/app")},
			path:     "/etc/hosts",
			opts:     Options{Legacy: true},
			wantKeep: true,
			reason:   "legacy mode also keeps unmatched paths",
		},
		{
			name:     "legacyIncludeKeepsWhenNoExclude",
			rules:    []Rule{inc("/app")},
			path:     "/app/x",
			opts:     Options{Legacy: true},
			wantKeep: true,
			reason:   "an include still keeps its paths when no exclude matches",
		},

		// ---- include protected by more specific include --------------------
		{
			name:     "specificIncludeProtectsSubtree",
			rules:    []Rule{exc("/app/**"), inc("/app/data/**"), exc("/app/data/tmp/**")},
			path:     "/app/data/keep.txt",
			wantKeep: true,
			reason:   "among several rules the deepest literal anchor wins",
		},
		{
			name:     "specificIncludeLosesToDeeperExclude",
			rules:    []Rule{exc("/app/**"), inc("/app/data/**"), exc("/app/data/tmp/**")},
			path:     "/app/data/tmp/scratch",
			wantKeep: false,
			reason:   "a deeper exclude still wins inside an included subtree",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := Compile(tc.rules, tc.opts)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			got := rs.Decide(tc.path, tc.isDir)
			if got.Keep != tc.wantKeep {
				t.Fatalf("Decide(%q, isDir=%v) keep=%v, want %v\nreason: %s\ndecision: %s\nmatches: %v",
					tc.path, tc.isDir, got.Keep, tc.wantKeep, tc.reason, got.Explanation, got.Matches)
			}

			if tc.wantKeep && got.Matched != nil && got.Matched.Action == Exclude {
				t.Fatalf("kept path attributed to an exclude rule: %v", got.Matched)
			}

			if !tc.wantKeep && len(tc.rules) > 0 && got.Matched != nil && got.Matched.Action == Include {
				t.Fatalf("removed path attributed to an include rule: %v", got.Matched)
			}
		})
	}
}

func TestDecisionExplainsWinner(t *testing.T) {
	rs, err := Compile([]Rule{
		{Pattern: "/var/log/**", Action: Exclude, Source: "flag:--exclude-pattern"},
		{Pattern: "/var/log/app/keep.log", Action: Include, Source: "file:rules.conf", Line: 7},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	got := rs.Decide("/var/log/app/keep.log", false)
	if !got.Keep {
		t.Fatalf("expected path kept, got %v", got.Explanation)
	}

	if got.Matched == nil || got.Matched.Pattern != "/var/log/app/keep.log" {
		t.Fatalf("unexpected winner: %+v", got.Matched)
	}

	if got.Matched.Source != "file:rules.conf" {
		t.Fatalf("winner source not preserved: %q", got.Matched.Source)
	}

	if len(got.Matches) != 2 {
		t.Fatalf("expected both matches to be reported, got %v", got.Matches)
	}

	if got.Explanation == "" {
		t.Fatal("expected a non-empty explanation")
	}

	t.Log(got.Explanation)
}

func TestPruneDirConsidersIncludes(t *testing.T) {
	rs, err := Compile([]Rule{exc("/var/log/**"), inc("/var/log/app/keep.log")}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if got := rs.Decide("/var/log", true); got.PruneDir {
		t.Fatalf("must not prune /var/log because an include may match below it: %s", got.Explanation)
	}

	if got := rs.Decide("/var/log/other", true); !got.PruneDir {
		t.Fatalf("expected /var/log/other to be prunable: %s", got.Explanation)
	}
}

func TestCompileRejectsBadRules(t *testing.T) {
	bad := []string{"", "/app/[", "/app/{a,b", "/app/a\\"}
	for _, pattern := range bad {
		if _, err := Compile([]Rule{exc(pattern)}, Options{}); err == nil {
			t.Fatalf("expected error for pattern %q", pattern)
		}
	}

	good := []string{"/app/[a-z]", "/app/{a,b}", "/app/a\\*", "/app/**", "/"}
	for _, pattern := range good {
		if _, err := Compile([]Rule{exc(pattern)}, Options{}); err != nil {
			t.Fatalf("unexpected error for pattern %q: %v", pattern, err)
		}
	}
}

func TestCompileLenientKeepsValidRules(t *testing.T) {
	rs, errs := CompileLenient([]Rule{exc("/app/["), exc("/app/secret")}, Options{})
	if len(errs) != 1 {
		t.Fatalf("expected one error, got %v", errs)
	}

	if rs.Size() != 1 {
		t.Fatalf("expected one compiled rule, got %d", rs.Size())
	}

	if rs.ShouldKeep("/app/secret", false) {
		t.Fatal("the valid rule must still apply")
	}
}

func TestRulesDeduplicated(t *testing.T) {
	rs, err := Compile([]Rule{
		{Pattern: "/app/x", Action: Exclude, Source: "flag"},
		{Pattern: "/app//x", Action: Exclude, Source: "file"},
		{Pattern: "/app/x", Action: Exclude, Source: "flag"},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if rs.Size() != 1 {
		t.Fatalf("expected identical rules to collapse, got %d", rs.Size())
	}

	rules := rs.Rules()
	if len(rules) != 1 {
		t.Fatalf("unexpected rule count: %d", len(rules))
	}

	if rules[0].Source != "file, flag" {
		t.Fatalf("origins not merged deterministically: %q", rules[0].Source)
	}

	// A directory-only rule is a distinct rule from the plain rule with the
	// same pattern, because it applies to a different set of paths.
	rs, err = Compile([]Rule{
		{Pattern: "/app/cache", Action: Exclude},
		{Pattern: "/app/cache/", Action: Exclude},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if rs.Size() != 2 {
		t.Fatalf("directory-only rule must not collapse into the plain rule, got %d", rs.Size())
	}
}

func TestLegacyEnv(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"1":     true,
		"true":  true,
		"yes":   true,
	}

	for value, want := range cases {
		got := legacyModeFromEnv(func(string) string { return value })
		if got != want {
			t.Fatalf("legacyModeFromEnv(%q)=%v, want %v", value, got, want)
		}
	}
}

// Benchmarks support the complexity claims in the package documentation:
// precompiled rules and index buckets keep matching close to linear in the
// number of paths for realistic rule sets. Run with:
//
//	go test ./pkg/util/filter -bench . -benchmem
func benchRuleSet(b *testing.B, ruleCount int) (*RuleSet, []string) {
	b.Helper()

	rules := make([]Rule, 0, ruleCount)
	for i := 0; i < ruleCount; i++ {
		switch i % 4 {
		case 0:
			rules = append(rules, exc(fmt.Sprintf("/app/mod%d/data/**", i)))
		case 1:
			rules = append(rules, inc(fmt.Sprintf("/app/mod%d/data/keep.log", i)))
		case 2:
			rules = append(rules, exc(fmt.Sprintf("/var/log/svc%d/**", i)))
		default:
			rules = append(rules, exc(fmt.Sprintf("/usr/lib/lib%d.so", i)))
		}
	}

	rs, err := Compile(rules, Options{})
	if err != nil {
		b.Fatal(err)
	}

	paths := make([]string, 0, 4096)
	for i := 0; i < 4096; i++ {
		paths = append(paths, fmt.Sprintf("/app/mod%d/data/file%d", i%ruleCount, i))
	}

	return rs, paths
}

func BenchmarkDecide100Rules(b *testing.B) {
	rs, paths := benchRuleSet(b, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rs.Decide(paths[i%len(paths)], false)
	}
}

func BenchmarkDecide500Rules(b *testing.B) {
	rs, paths := benchRuleSet(b, 500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rs.Decide(paths[i%len(paths)], false)
	}
}

// Rules that normalize to the same pattern but differ in their trailing slash
// or spelling must resolve deterministically regardless of input order.
func TestEquivalentRulesDeterministic(t *testing.T) {
	variants := [][]Rule{
		{exc("/app/cache"), exc("/app/cache/")},
		{exc("/app/cache/"), exc("/app/cache")},
		{exc("/app//cache"), exc("/app/cache/")},
		{exc("/app/./cache/"), exc("/app/cache")},
	}

	var first *Decision
	for _, rules := range variants {
		rs, err := Compile(rules, Options{})
		if err != nil {
			t.Fatal(err)
		}

		d := rs.Decide("/app/cache", true)
		if first == nil {
			first = &d
			continue
		}

		if d.Keep != first.Keep || d.Explanation != first.Explanation {
			t.Fatalf("equivalent rule sets disagree:\n%v\nvs\n%v", *first, d)
		}
	}
}
