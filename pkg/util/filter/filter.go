// Package filter implements deterministic include/exclude path filtering for
// container artifacts.
//
// # Semantics
//
// Matching is done on absolute, slash-separated container paths. A pattern is
// matched against a path and, if the pattern matches one of the path's
// ancestor directories, it also applies to the path (a rule that matches a
// directory applies to its contents). Paths and patterns are normalized with
// path.Clean.
//
// Wildcards follow de-facto shell/doublestar expectations:
//
//   - matches any sequence of characters except '/'
//     **     matches any sequence of characters including '/'
//     ?      matches any single character except '/'
//     [a-z]  character class with ranges
//     [^...] negated character class
//     [!...] negated character class (alternate syntax)
//     {a,b}  alternation
//     \x     escapes a meta character
//
// A pattern ending in '/' is a directory-only rule. A pattern ending in '/**'
// (a whole trailing component) is a subtree rule anchored at the directory
// before the doublestar; this works even when the anchor contains wildcards.
//
// Dot files are matched by wildcards (the pre-existing behavior of the
// doublestar package used by the sensor), e.g. "/app/*" matches
// "/app/.env". A leading dot has no special meaning.
//
// Relative patterns are resolved against Options.Base (default "/", the
// container root), never against the host process working directory.
//
// # Conflict resolution
//
// When more than one rule matches a path the most specific rule wins. A rule
// is more specific when it has a deeper literal anchor (more literal path
// segments before the first wildcard), then more literal characters, then
// fewer "**" segments, then more segments. If two matching rules have the same
// specificity, a rule that applies to the path itself (direct match) wins over
// a rule that only matches one of its ancestor directories. If they are still
// tied, an include rule wins over an exclude rule. Remaining ties are broken
// by the lexicographically smaller normalized pattern, so the outcome does
// not depend on rule order.
//
// This gives the intended behavior for the common configurations:
//
//   - exclude "/var/log/**" + include "/var/log/app/keep.log" -> the file is
//     kept (the include rule has a deeper literal anchor);
//   - include "/app" + exclude "/app/testdata/**" -> test data is excluded
//     (the exclude rule is anchored deeper) while the rest of /app is kept.
//
// # Legacy semantics
//
// Options.Legacy (or SLIM_FILTER_LEGACY_SEMANTICS=1 through
// LegacyModeFromEnv) restores the previous behavior where any matching
// exclude removed the path and includes never overrode excludes. With
// exclude-only rule sets the new semantics and the legacy semantics produce
// the same results.
//
// # Complexity
//
// Rules are validated and precompiled once. Exact patterns, literal subtree
// anchors and glob patterns are indexed separately by their first literal
// segment, so matching a path only consults the rules that can possibly match
// it. With R rules and P paths the worst case remains O(P * R * D) (D is the
// path depth; this happens when many "**"-leading rules are configured), but
// for the common case with literal anchors the cost is O(P * (D + B)) where B
// is the number of glob rules sharing the first path segment. Compilation is
// O(R * L) time and O(R) memory for L total pattern length.
package filter

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v3"
)

// EnvLegacySemantics is the environment variable that switches the filter
// engine (and its sensor integration) back to the pre-existing matching
// semantics. Any non-empty value other than "0" and "false" enables it.
const EnvLegacySemantics = "SLIM_FILTER_LEGACY_SEMANTICS"

// LegacyModeFromEnv reports whether the legacy filter semantics were
// requested through the environment.
func LegacyModeFromEnv() bool {
	return legacyModeFromEnv(osGetenv)
}

// Action identifies what a rule does with a matching path.
type Action uint8

const (
	// Exclude removes a matching path from the result.
	Exclude Action = iota
	// Include keeps a matching path in the result.
	Include
)

func (a Action) String() string {
	switch a {
	case Include:
		return "include"
	case Exclude:
		return "exclude"
	default:
		return fmt.Sprintf("action(%d)", uint8(a))
	}
}

// Rule is an uncompiled filtering rule.
type Rule struct {
	// Pattern is an absolute or Base-relative path pattern.
	Pattern string
	// Action decides whether matching paths are kept or removed.
	Action Action
	// Source is a human readable origin of the rule (e.g. "flag:--exclude-pattern"
	// or "file:/etc/slim/rules.conf:12"). It is only used in explanations.
	Source string
	// Line optionally records the source line for explanations.
	Line int
}

func (r Rule) String() string {
	origin := r.Source
	if r.Line > 0 {
		if origin == "" {
			origin = fmt.Sprintf("line %d", r.Line)
		} else {
			origin = fmt.Sprintf("%s:%d", origin, r.Line)
		}
	}

	if origin == "" {
		return fmt.Sprintf("%s %q", r.Action, r.Pattern)
	}

	return fmt.Sprintf("%s %q (%s)", r.Action, r.Pattern, origin)
}

// Options configures a RuleSet.
type Options struct {
	// Base is the base directory for relative patterns. Defaults to "/".
	Base string
	// DropUnmatched removes paths that no rule matched. The zero value keeps
	// unmatched paths, which is what artifact filtering needs.
	DropUnmatched bool
	// Legacy restores the pre-existing semantics: any matching exclude wins
	// and includes cannot override excludes.
	Legacy bool
}

func (o Options) normalized() Options {
	if o.Base == "" {
		o.Base = "/"
	}

	if !strings.HasPrefix(o.Base, "/") {
		o.Base = "/" + o.Base
	}

	o.Base = path.Clean(o.Base)
	if o.Base == "." {
		o.Base = "/"
	}

	return o
}

// ErrEmptyPattern is returned when a rule has an empty pattern.
var ErrEmptyPattern = errors.New("empty filter pattern")

// Specificity is the order-independent specificity vector used to resolve
// conflicts. Larger values are more specific.
type Specificity struct {
	// PrefixSegments is the number of leading path segments without wildcards.
	PrefixSegments int
	// LiteralChars is the number of literal characters in the pattern.
	LiteralChars int
	// NonDoublestarSegments is the number of segments that are not "**".
	NonDoublestarSegments int
	// Segments is the total number of path segments.
	Segments int
	// Exact reports that the pattern has no wildcard characters.
	Exact bool
}

func (s Specificity) String() string {
	return fmt.Sprintf("prefix=%d literal=%d non**=%d segments=%d exact=%v",
		s.PrefixSegments, s.LiteralChars, s.NonDoublestarSegments, s.Segments, s.Exact)
}

// MatchInfo describes one rule that matched a path.
type MatchInfo struct {
	// Pattern is the normalized absolute pattern.
	Pattern string
	// Action is the action of the matching rule.
	Action Action
	// Source is the optional rule origin.
	Source string
	// Specificity is the rule's specificity vector.
	Specificity Specificity
	// Direct is true when the rule matched the path itself (including subtree
	// rules whose anchor covers the path) rather than only one of its ancestor
	// directories.
	Direct bool
	// DirOnly is true for a rule written with a trailing slash. It only
	// distinguishes rules whose normalized pattern is otherwise identical, so
	// the outcome stays order-independent.
	DirOnly bool
}

func (m MatchInfo) String() string {
	mode := "ancestor"
	if m.Direct {
		mode = "direct"
	}

	return fmt.Sprintf("%s %q [%s, %s]", m.Action, m.Pattern, mode, m.Specificity)
}

// Decision is the result of filtering one path.
type Decision struct {
	// Keep reports whether the path must be included in the result.
	Keep bool
	// Matched is the rule that determined the decision. It is nil when no rule
	// matched and the default was used.
	Matched *MatchInfo
	// Matches lists every rule that matched the path, ordered from the most
	// specific to the least specific. It is capped to keep decisions small.
	Matches []MatchInfo
	// Explanation is a stable, human readable description of the decision.
	Explanation string
	// PruneDir is true when the path is a directory that is excluded and no
	// include rule can possibly keep anything below it, so a walker may skip
	// the whole subtree.
	PruneDir bool
}

type compiledRule struct {
	rule Rule
	// raw is the pattern exactly as supplied by the user. Legacy mode matches
	// it verbatim to preserve the pre-existing results.
	raw string
	// pattern is the normalized absolute pattern (without a trailing slash).
	pattern string
	// dirOnly is set for patterns ending in '/'.
	dirOnly bool
	// subtree is set for patterns ending in '/**'.
	subtree bool
	// anchor is the literal or glob directory anchor of a subtree rule.
	anchor string
	// anchorGlob reports that the subtree anchor contains wildcards.
	anchorGlob bool
	// anchorPrefix is the literal prefix of the subtree anchor ("/" when the
	// anchor starts with a wildcard).
	anchorPrefix string
	// literalPrefix is the literal directory prefix of the pattern ("/" when
	// the first segment has wildcards).
	literalPrefix string
	// literalKey is the exact path an exact (wildcard-free), non-subtree rule
	// is indexed under.
	literalKey string
	// firstSegment is the key used to index the rule ("" when the first
	// segment has wildcards).
	firstSegment string
	hasMeta      bool
	spec         Specificity
}

// dedupeKey identifies rules with the same normalized semantics. Identical
// rules are collapsed, and a trailing-slash (directory-only) rule is distinct
// from the plain rule with the same pattern.
func (c *compiledRule) dedupeKey() string {
	key := c.rule.Action.String() + "\x00" + c.pattern
	if c.subtree {
		key += "\x00subtree"
	}

	if c.dirOnly {
		key += "\x00dir"
	}

	return key
}

// RuleSet is a compiled, immutable set of filtering rules.
type RuleSet struct {
	opts Options

	rules []compiledRule

	exact      map[string][]int
	subtree    map[string][]int
	globBucket map[string][]int
	// includeRules holds indexes of include rules, used to decide whether an
	// excluded directory can be pruned.
	includeRules []int
}

// Compile validates and precompiles the rules. A malformed rule (empty
// pattern or a malformed glob) is reported as an error; use CompileLenient to
// keep the valid rules instead.
func Compile(rules []Rule, opts Options) (*RuleSet, error) {
	rs, errs := CompileLenient(rules, opts)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return rs, nil
}

// CompileLenient compiles every valid rule and returns the errors for the
// rules that were skipped. It never returns a nil RuleSet.
func CompileLenient(rules []Rule, opts Options) (*RuleSet, []error) {
	opts = opts.normalized()

	rs := &RuleSet{
		opts:       opts,
		exact:      map[string][]int{},
		subtree:    map[string][]int{},
		globBucket: map[string][]int{},
	}

	var errs []error
	seen := map[string]int{} // pattern+"\x00"+action -> index

	for _, r := range rules {
		cr, err := compileRule(r, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("filter rule %q: %w", r.Pattern, err))
			continue
		}

		key := cr.dedupeKey()
		if idx, found := seen[key]; found && !opts.Legacy {
			// Deduplicate identical rules and merge their origins so the
			// result stays deterministic regardless of input order.
			existing := &rs.rules[idx]
			existing.rule.Source = mergeSources(existing.rule.Source, r.Source)
			continue
		}

		idx := len(rs.rules)
		seen[key] = idx
		rs.rules = append(rs.rules, cr)

		switch {
		case cr.subtree && !cr.anchorGlob:
			rs.subtree[cr.anchor] = append(rs.subtree[cr.anchor], idx)
		case !cr.hasMeta:
			rs.exact[cr.literalKey] = append(rs.exact[cr.literalKey], idx)
		default:
			rs.globBucket[cr.firstSegment] = append(rs.globBucket[cr.firstSegment], idx)
		}

		if cr.rule.Action == Include {
			rs.includeRules = append(rs.includeRules, idx)
		}
	}

	return rs, errs
}

func mergeSources(a, b string) string {
	if a == "" || a == b {
		return b
	}

	if b == "" {
		return a
	}

	parts := strings.Split(a, ", ")
	for _, p := range parts {
		if p == b {
			return a
		}
	}

	parts = append(parts, b)
	sort.Strings(parts)

	return strings.Join(parts, ", ")
}

func compileRule(r Rule, opts Options) (compiledRule, error) {
	raw := r.Pattern
	if raw == "" {
		return compiledRule{}, ErrEmptyPattern
	}

	dirOnly := strings.HasSuffix(raw, "/")
	trimmed := strings.TrimRight(raw, "/")
	if trimmed == "" {
		// The pattern was just slashes; treat it as the base directory.
		trimmed = "/"
	}

	var normalized string
	if strings.HasPrefix(trimmed, "/") {
		normalized = path.Clean(trimmed)
	} else {
		normalized = path.Join(opts.Base, trimmed)
	}

	if normalized == "." {
		normalized = "/"
	}

	cr := compiledRule{
		rule:    r,
		raw:     raw,
		pattern: normalized,
		dirOnly: dirOnly,
		hasMeta: patternHasMeta(normalized),
	}

	// A trailing '/**' component is a subtree rule anchored at the directory
	// before it. This is what makes "/dir/**" cover "/dir" itself as well as
	// its descendants (doublestar alone does not match "/dir").
	if normalized == "/**" {
		cr.subtree = true
		cr.anchor = "/"
	} else if strings.HasSuffix(normalized, "/**") {
		cr.subtree = true
		cr.anchor = strings.TrimSuffix(normalized, "/**")
		if cr.anchor == "" {
			cr.anchor = "/"
		}
	}

	cr.rule.Pattern = normalized

	if cr.subtree {
		cr.anchorGlob = patternHasMeta(cr.anchor)
		cr.anchorPrefix = unescapeLiteral(literalPrefix(cr.anchor))
		cr.spec = specificityOf(cr.anchor)
		if cr.anchorGlob {
			cr.firstSegment = firstSegmentOf(cr.anchorPrefix)
		} else {
			// The anchor is compared literally, so escaped metacharacters must
			// be unescaped first.
			cr.anchor = unescapeLiteral(cr.anchor)
		}
	} else {
		cr.literalPrefix = unescapeLiteral(literalPrefix(normalized))
		cr.firstSegment = firstSegmentOf(cr.literalPrefix)
		cr.spec = specificityOf(normalized)

		if !cr.hasMeta {
			if cr.dirOnly {
				// Literal directory-only rules are consulted for every path
				// below their directory, so they live in the subtree index.
				cr.subtree = true
				cr.anchor = unescapeLiteral(normalized)
				cr.anchorPrefix = cr.anchor
			} else {
				cr.literalKey = unescapeLiteral(normalized)
				cr.pattern = cr.literalKey
				cr.rule.Pattern = cr.literalKey
			}
		}
	}

	// Validate the glob early so a bad rule is rejected at compile time
	// instead of silently never matching (or being ignored mid-walk). The
	// original normalized pattern is validated before any unescaping.
	if err := validatePattern(normalized); err != nil {
		return compiledRule{}, err
	}

	return cr, nil
}

// validatePattern rejects malformed patterns. doublestar.Match only parses the
// parts of a pattern it needs to reach, so a bare Match(pattern, "") can miss
// a malformed class or brace. A hand written well-formedness scan plus a match
// against a synthesized concrete name covers both cases.
func validatePattern(pattern string) error {
	synthetic, err := scanPattern(pattern)
	if err != nil {
		return err
	}

	if _, err := doublestar.Match(pattern, synthetic); err != nil {
		return err
	}

	return nil
}

// scanPattern walks the pattern and returns a concrete name it should match
// ('x' for wildcards and classes, 'x' for the first alternative of a brace
// group). It returns an error for a malformed class, brace or trailing escape.
func scanPattern(pattern string) (string, error) {
	var b strings.Builder
	runes := []rune(pattern)

	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			if i+1 >= len(runes) {
				return "", doublestar.ErrBadPattern
			}

			i++
			b.WriteRune(runes[i])
		case '*', '?':
			b.WriteByte('x')
		case '[':
			next, err := scanClass(runes, i+1)
			if err != nil {
				return "", err
			}

			i = next
			b.WriteByte('x')
		case '{':
			next, err := scanAlternation(runes, i+1)
			if err != nil {
				return "", err
			}

			i = next
			// An empty alternative ({,x}) can match zero characters; 'x' keeps
			// the synthesized name matchable for non-empty alternatives too.
			b.WriteByte('x')
		default:
			b.WriteRune(runes[i])
		}
	}

	return b.String(), nil
}

// scanClass consumes a character class starting after '['. It returns the
// index of the closing ']'.
func scanClass(runes []rune, i int) (int, error) {
	if i < len(runes) && (runes[i] == '^' || runes[i] == '!') {
		i++
	}

	start := i
	for ; i < len(runes) && runes[i] != ']'; i++ {
		if runes[i] == '\\' {
			i++
			if i >= len(runes) {
				return 0, doublestar.ErrBadPattern
			}
		}
	}

	if i >= len(runes) || i == start {
		return 0, doublestar.ErrBadPattern
	}

	// A leading or trailing '-' and a dangling range are malformed. doublestar
	// reports these too, but the synthesized name below cannot exercise every
	// class branch, so they are checked here.
	if runes[start] == '-' || runes[i-1] == '-' {
		return 0, doublestar.ErrBadPattern
	}

	for j := start; j < i; j++ {
		if runes[j] == '\\' {
			j++
			continue
		}

		if runes[j] == '-' && (j+1 >= i || runes[j+1] == '-') {
			return 0, doublestar.ErrBadPattern
		}
	}

	return i, nil
}

// scanAlternation consumes a brace alternation starting after '{'. It returns
// the index of the matching '}'.
func scanAlternation(runes []rune, i int) (int, error) {
	depth := 1
	for ; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			i++
			if i >= len(runes) {
				return 0, doublestar.ErrBadPattern
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}

	return 0, doublestar.ErrBadPattern
}

// matchPattern returns the pattern used for doublestar matching.
func (c *compiledRule) matchPattern() string {
	if c.subtree {
		return c.anchor
	}

	return c.pattern
}

func unescapeLiteral(p string) string {
	if !strings.Contains(p, "\\") {
		return p
	}

	var b strings.Builder
	escaped := false
	for _, r := range p {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}

		if r == '\\' {
			escaped = true
			continue
		}

		b.WriteRune(r)
	}

	return b.String()
}

func patternHasMeta(s string) bool {
	escaped := false
	for _, r := range s {
		if escaped {
			escaped = false
			continue
		}

		switch r {
		case '\\':
			escaped = true
		case '*', '?', '[', '{':
			return true
		}
	}

	return false
}

// pathSegments splits an absolute, cleaned path into segments without the
// leading empty segment.
func pathSegments(p string) []string {
	if p == "/" || p == "" {
		return nil
	}

	return strings.Split(strings.TrimPrefix(p, "/"), "/")
}

// literalPrefix returns the longest leading sequence of segments that contain
// no wildcards, as an absolute path. It returns "/" when the first segment has
// wildcards.
func literalPrefix(p string) string {
	segs := pathSegments(p)
	var literal []string
	for _, seg := range segs {
		if patternHasMeta(seg) {
			break
		}

		literal = append(literal, seg)
	}

	if len(literal) == 0 {
		return "/"
	}

	return "/" + strings.Join(literal, "/")
}

func firstSegmentOf(prefix string) string {
	segs := pathSegments(prefix)
	if len(segs) == 0 {
		return ""
	}

	return segs[0]
}

func specificityOf(pattern string) Specificity {
	segs := pathSegments(pattern)
	spec := Specificity{
		Segments: len(segs),
		Exact:    !patternHasMeta(pattern),
	}

	prefixOpen := true
	for _, seg := range segs {
		if prefixOpen && !patternHasMeta(seg) {
			spec.PrefixSegments++
		} else {
			prefixOpen = false
		}

		if seg != "**" {
			spec.NonDoublestarSegments++
		}

		spec.LiteralChars += literalCharCount(seg)
	}

	return spec
}

func literalCharCount(seg string) int {
	count := 0
	escaped := false
	for _, r := range seg {
		if escaped {
			count++
			escaped = false
			continue
		}

		switch r {
		case '\\':
			escaped = true
		case '*', '?', '[', ']', '{', '}':
			// meta character, not literal
		default:
			count++
		}
	}

	return count
}

// compareSpecificity returns >0 when a is more specific than b.
func compareSpecificity(a, b Specificity) int {
	if a.PrefixSegments != b.PrefixSegments {
		return a.PrefixSegments - b.PrefixSegments
	}

	if a.LiteralChars != b.LiteralChars {
		return a.LiteralChars - b.LiteralChars
	}

	if a.NonDoublestarSegments != b.NonDoublestarSegments {
		return a.NonDoublestarSegments - b.NonDoublestarSegments
	}

	if a.Segments != b.Segments {
		return a.Segments - b.Segments
	}

	if a.Exact != b.Exact {
		if a.Exact {
			return 1
		}

		return -1
	}

	return 0
}

// compareMatch orders two matching rules. It returns >0 when a wins.
func compareMatch(a, b MatchInfo) int {
	if c := compareSpecificity(a.Specificity, b.Specificity); c != 0 {
		return c
	}

	if a.Direct != b.Direct {
		if a.Direct {
			return 1
		}

		return -1
	}

	if a.Action != b.Action {
		if a.Action == Include {
			return 1
		}

		return -1
	}

	// Rules that differ only by their trailing slash (directory-only) or their
	// original spelling must still resolve deterministically, independent of
	// rule order.
	if a.DirOnly != b.DirOnly {
		if a.DirOnly {
			return 1
		}

		return -1
	}

	return -strings.Compare(a.Pattern, b.Pattern)
}

// Decide filters one path. isDir must describe the path itself; a rule that
// matches an ancestor directory always applies to the path as a directory
// match.
func (rs *RuleSet) Decide(p string, isDir bool) Decision {
	if rs.opts.Legacy {
		return rs.decideLiteralLegacy(p, isDir)
	}

	p = rs.normalizePath(p)

	if len(rs.rules) == 0 {
		keep := !rs.opts.DropUnmatched
		return Decision{
			Keep:        keep,
			Explanation: "no rules configured: " + keepWord(keep),
		}
	}

	matches := rs.collectMatches(p, isDir)

	if len(matches) == 0 {
		keep := !rs.opts.DropUnmatched
		return Decision{
			Keep:        keep,
			Explanation: "no rule matched: " + keepWord(keep),
		}
	}

	decision := Decision{
		Matches: capMatches(matches),
	}

	winnerIdx := 0
	for i := 1; i < len(matches); i++ {
		if compareMatch(matches[i], matches[winnerIdx]) > 0 {
			winnerIdx = i
		}
	}

	winner := matches[winnerIdx]
	decision.Keep = winner.Action == Include
	decision.Matched = &winner
	decision.Explanation = rs.explainWinner(winner, matches)

	if !decision.Keep && isDir {
		decision.PruneDir = !rs.includeMayMatchBelow(p)
	}

	return decision
}

// ShouldKeep is a convenience wrapper around Decide.
func (rs *RuleSet) ShouldKeep(p string, isDir bool) bool {
	return rs.Decide(p, isDir).Keep
}

// decideLiteralLegacy reproduces the pre-existing matcher exactly: each
// exclude pattern is matched verbatim against the path (no normalization and no
// ancestor propagation), and any match removes the path. Includes never
// override an exclude. This is what keeps existing user rule files producing
// their previous results under SLIM_FILTER_LEGACY_SEMANTICS=1.
func (rs *RuleSet) decideLiteralLegacy(p string, isDir bool) Decision {
	var matchedExclude, matchedInclude *Rule

	for i := range rs.rules {
		cr := &rs.rules[i]
		ok, err := doublestar.Match(cr.raw, p)
		if err != nil || !ok {
			continue
		}

		if cr.rule.Action == Exclude {
			if matchedExclude == nil {
				rule := Rule{Pattern: cr.raw, Action: cr.rule.Action, Source: cr.rule.Source, Line: cr.rule.Line}
				matchedExclude = &rule
			}

			continue
		}

		if matchedInclude == nil {
			rule := cr.rule
			matchedInclude = &rule
		}
	}

	decision := Decision{
		Keep:     matchedExclude == nil && (!rs.opts.DropUnmatched || matchedInclude != nil),
		PruneDir: matchedExclude != nil && isDir,
	}

	if matchedExclude != nil {
		rule := *matchedExclude
		decision.Matched = &MatchInfo{
			Pattern:     rule.Pattern,
			Action:      Exclude,
			Source:      rule.Source,
			Specificity: specificityOf(rule.Pattern),
			Direct:      true,
		}
		decision.Explanation = fmt.Sprintf(
			"legacy semantics: exclude %q matches the path directly; excludes always win over includes", rule.Pattern)
		return decision
	}

	if matchedInclude != nil {
		rule := *matchedInclude
		decision.Matched = &MatchInfo{
			Pattern:     rule.Pattern,
			Action:      Include,
			Source:      rule.Source,
			Specificity: specificityOf(rule.Pattern),
			Direct:      true,
		}
		decision.Explanation = fmt.Sprintf(
			"legacy semantics: no exclude matched; include %q keeps the path", rule.Pattern)
		return decision
	}

	decision.Explanation = "legacy semantics: no rule matched: " + keepWord(decision.Keep)
	return decision
}

func (rs *RuleSet) explainWinner(winner MatchInfo, matches []MatchInfo) string {
	losers := make([]MatchInfo, 0, len(matches))
	for _, m := range matches {
		if m.Pattern == winner.Pattern && m.Action == winner.Action {
			continue
		}

		losers = append(losers, m)
	}

	if len(losers) == 0 {
		return fmt.Sprintf("%s wins: only matching rule", winner.String())
	}

	sort.SliceStable(losers, func(i, j int) bool {
		return compareMatch(losers[i], losers[j]) > 0
	})

	const maxLosers = 3
	if len(losers) > maxLosers {
		losers = losers[:maxLosers]
	}

	reasons := make([]string, 0, len(losers))
	for _, l := range losers {
		reasons = append(reasons, l.String())
	}

	return fmt.Sprintf("%s wins over %s", winner.String(), strings.Join(reasons, "; "))
}

func (rs *RuleSet) normalizePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = path.Join(rs.opts.Base, p)
	}

	p = path.Clean(p)
	if p == "." {
		p = "/"
	}

	return p
}

func keepWord(keep bool) string {
	if keep {
		return "kept"
	}

	return "removed"
}

func capMatches(matches []MatchInfo) []MatchInfo {
	sorted := make([]MatchInfo, len(matches))
	copy(sorted, matches)
	sort.SliceStable(sorted, func(i, j int) bool {
		return compareMatch(sorted[i], sorted[j]) > 0
	})

	const maxMatches = 16
	if len(sorted) > maxMatches {
		sorted = sorted[:maxMatches]
	}

	return sorted
}

// collectMatches returns all rules that match p. The order of the returned
// slice is stable but not necessarily sorted; callers sort by precedence.
func (rs *RuleSet) collectMatches(p string, isDir bool) []MatchInfo {
	var out []MatchInfo
	seen := map[int]struct{}{}

	add := func(idx int, direct bool) {
		if _, found := seen[idx]; found {
			return
		}

		seen[idx] = struct{}{}
		cr := &rs.rules[idx]
		out = append(out, MatchInfo{
			Pattern:     cr.pattern,
			Action:      cr.rule.Action,
			Source:      cr.rule.Source,
			Specificity: cr.spec,
			Direct:      direct,
			DirOnly:     cr.dirOnly,
		})
	}

	ancestors := ancestorPaths(p)

	for _, idx := range rs.exact[p] {
		add(idx, true)
	}

	// Ancestor matches: a rule that matches a parent directory covers the
	// path. The path itself is handled above (and exempted from dir-only
	// rules), so it is skipped here.
	for _, a := range ancestors[1:] {
		for _, idx := range rs.exact[a] {
			add(idx, false)
		}
	}

	for _, a := range ancestors {
		for _, idx := range rs.subtree[a] {
			cr := &rs.rules[idx]
			direct, matched := cr.match(p, isDir)
			if matched {
				add(idx, direct)
			}
		}
	}

	first := firstSegmentOf(p)
	buckets := []string{""}
	if first != "" {
		buckets = append([]string{first}, buckets...)
	}

	for _, key := range buckets {
		for _, idx := range rs.globBucket[key] {
			cr := &rs.rules[idx]
			direct, matched := cr.match(p, isDir)
			if matched {
				add(idx, direct)
			}
		}
	}

	return out
}

// ancestorPaths returns p itself followed by each parent directory up to and
// including "/".
func ancestorPaths(p string) []string {
	paths := []string{p}
	cur := p
	for cur != "/" && cur != "." && cur != "" {
		cur = path.Dir(cur)
		paths = append(paths, cur)
	}

	return paths
}

// match reports whether the rule matches p and whether the match applies to p
// itself (direct) or only to an ancestor directory.
func (c *compiledRule) match(p string, isDir bool) (direct bool, matched bool) {
	if c.subtree {
		return c.matchSubtree(p, isDir)
	}

	if !prefixCanMatch(c.literalPrefix, p) {
		return false, false
	}

	if ok := c.matchOne(p); ok && (!c.dirOnly || isDir) {
		return true, true
	}

	for _, a := range ancestorPaths(p)[1:] {
		if c.matchOne(a) {
			return false, true
		}
	}

	return false, false
}

func (c *compiledRule) matchSubtree(p string, isDir bool) (direct bool, matched bool) {
	anchored := func(a string) bool {
		if p == a {
			// A trailing-slash rule only applies to directories; it still
			// covers everything below the directory.
			return !c.dirOnly || isDir
		}

		return strings.HasPrefix(p, a+"/") || (a == "/" && strings.HasPrefix(p, "/"))
	}

	if !c.anchorGlob {
		if anchored(c.anchor) {
			return true, true
		}

		return false, false
	}

	for _, a := range ancestorPaths(p) {
		if ok, err := doublestar.Match(c.anchor, a); err == nil && ok {
			if a == p && c.dirOnly && !isDir {
				continue
			}

			return true, true
		}
	}

	return false, false
}

func (c *compiledRule) matchOne(p string) bool {
	ok, err := doublestar.Match(c.pattern, p)
	return err == nil && ok
}

// prefixCanMatch reports whether p can possibly be matched by a pattern whose
// literal prefix is prefix. It is a cheap prefilter.
func prefixCanMatch(prefix, p string) bool {
	if prefix == "/" {
		return true
	}

	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// includeMayMatchBelow reports whether any include rule can match dir or one
// of its descendants. It is conservative: it returns true when unsure.
func (rs *RuleSet) includeMayMatchBelow(dir string) bool {
	for _, idx := range rs.includeRules {
		cr := &rs.rules[idx]

		prefix := cr.literalPrefix
		if cr.subtree {
			prefix = cr.anchorPrefix
			if !cr.anchorGlob {
				if cr.anchor == dir || strings.HasPrefix(cr.anchor, dir+"/") {
					return true
				}
			}
		}

		if prefix == "/" ||
			prefix == dir ||
			strings.HasPrefix(prefix, dir+"/") ||
			strings.HasPrefix(dir, prefix+"/") {
			return true
		}
	}

	return false
}

// Rules returns a copy of the normalized rules in the set.
func (rs *RuleSet) Rules() []Rule {
	rules := make([]Rule, 0, len(rs.rules))
	for i := range rs.rules {
		rules = append(rules, rs.rules[i].rule)
	}

	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Action != rules[j].Action {
			return rules[i].Action == Include
		}

		return rules[i].Pattern < rules[j].Pattern
	})

	return rules
}

// Size returns the number of compiled rules.
func (rs *RuleSet) Size() int {
	return len(rs.rules)
}
