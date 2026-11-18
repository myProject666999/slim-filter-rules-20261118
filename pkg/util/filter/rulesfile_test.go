package filter

import (
	"strings"
	"testing"
)

func TestParseRulesKeywords(t *testing.T) {
	data := `
# comment
include /app/data
exclude /app/testdata/**
-bare.negated
+bare.positive

/plain/pattern
/app/x   # trailing comment
`
	rules, errs := ParseRulesString(data, "rules.conf")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	want := []struct {
		pattern string
		action  Action
		line    int
	}{
		{"/app/data", Include, 3},
		{"/app/testdata/**", Exclude, 4},
		{"bare.negated", Include, 5},
		{"bare.positive", Include, 6},
		{"/plain/pattern", Exclude, 8},
		{"/app/x", Exclude, 9},
	}

	if len(rules) != len(want) {
		t.Fatalf("rules=%v", rules)
	}

	for i, w := range want {
		if rules[i].Pattern != w.pattern || rules[i].Action != w.action || rules[i].Line != w.line {
			t.Fatalf("rule[%d]=%+v, want %+v", i, rules[i], w)
		}

		if rules[i].Source != "rules.conf" {
			t.Fatalf("rule[%d] source=%q", i, rules[i].Source)
		}
	}
}

func TestParseRulesPlainPatternListIsExclude(t *testing.T) {
	// A plain newline-delimited list (the pre-existing input style) must keep
	// its meaning: every line is an exclude.
	rules, errs := ParseRulesString("/var/log/**\n/app/testdata/**\n", "patterns.txt")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	for _, r := range rules {
		if r.Action != Exclude {
			t.Fatalf("expected exclude, got %v", r)
		}
	}
}

func TestParseRulesErrors(t *testing.T) {
	_, errs := ParseRulesString("include \nexclude /app\n", "rules.conf")
	if len(errs) != 1 {
		t.Fatalf("expected one error, got %v", errs)
	}

	if !strings.Contains(errs[0].Error(), "rules.conf:1") {
		t.Fatalf("error must include the location: %v", errs[0])
	}

	rs, compileErrs := CompileLenient([]Rule{{Pattern: "/app/[", Action: Exclude}}, Options{})
	if len(compileErrs) != 1 {
		t.Fatalf("expected compile error, got %v", compileErrs)
	}

	if rs.Size() != 0 {
		t.Fatalf("malformed rule must not be compiled: %d", rs.Size())
	}
}
