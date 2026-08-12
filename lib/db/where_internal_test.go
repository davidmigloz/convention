package db

import (
	"errors"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func Test_where_preservesLatchedError(t *testing.T) {
	sentinel := errors.New("sentinel")

	tests := []struct {
		name string
		call func(w *where)
	}{
		{"Noop", func(w *where) { w.Noop() }},
		{"Key", func(w *where) { w.Key("k") }},
		{"Equals", func(w *where) { w.Equals() }},
		{"NotEquals", func(w *where) { w.NotEquals() }},
		{"IsNull", func(w *where) { w.IsNull() }},
		{"IsNotNull", func(w *where) { w.IsNotNull() }},
		{"GreaterThan", func(w *where) { w.GreaterThan() }},
		{"GreaterThanOrEquals", func(w *where) { w.GreaterThanOrEquals() }},
		{"LessThan", func(w *where) { w.LessThan() }},
		{"LessThanOrEquals", func(w *where) { w.LessThanOrEquals() }},
		{"Like", func(w *where) { w.Like() }},
		{"In", func(w *where) { w.In() }},
		{"NotIn", func(w *where) { w.NotIn() }},
		{"Value", func(w *where) { w.Value(1) }},
		{"Values", func(w *where) { w.Values(1, 2) }},
		{"Or", func(w *where) { w.Or() }},
		{"And", func(w *where) { w.And() }},
		{"Search", func(w *where) { w.Search("text") }},
		{"CreatedBetween", func(w *where) { w.CreatedBetween(time.Time{}, time.Time{}) }},
		{"CreatedBy", func(w *where) { w.CreatedBy("user") }},
		{"UpdatedBetween", func(w *where) { w.UpdatedBetween(time.Time{}, time.Time{}) }},
		{"UpdatedBy", func(w *where) { w.UpdatedBy("user") }},
		{"Expression", func(w *where) { w.Expression(Where().Noop()) }},
		{"OrderByAsc", func(w *where) { w.OrderByAsc("k") }},
		{"OrderByDesc", func(w *where) { w.OrderByDesc("k") }},
		{"OrderByCreatedAtAsc", func(w *where) { w.OrderByCreatedAtAsc() }},
		{"OrderByCreatedAtDesc", func(w *where) { w.OrderByCreatedAtDesc() }},
		{"OrderByUpdatedAtAsc", func(w *where) { w.OrderByUpdatedAtAsc() }},
		{"OrderByUpdatedAtDesc", func(w *where) { w.OrderByUpdatedAtDesc() }},
		{"LimitPerShard", func(w *where) { w.LimitPerShard(10) }},
		{"Offset", func(w *where) { w.Offset(5) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &where{err: sentinel}
			tt.call(w)
			query, params, err := w.statement()
			if err != sentinel {
				t.Fatalf("latched error was clobbered: got %v", err)
			}
			if query != "" {
				t.Fatalf("wrote to query after error was latched: %q", query)
			}
			if len(params) != 0 {
				t.Fatalf("appended params after error was latched: %v", params)
			}
		})
	}
}

func Test_where_emptyKey(t *testing.T) {
	tests := []struct {
		name  string
		build func() whereReady
	}{
		{
			"first_statement",
			func() whereReady { return Where().Key("").Equals().Value(1) },
		},
		{
			"after_logical_operator",
			func() whereReady { return Where().Key("x").Equals().Value(1).And().Key("").Equals().Value(2) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.build().statement()
			if err == nil {
				t.Fatal("expected error for empty key, got nil")
			}
			if err.Error() != "key cannot be empty" {
				t.Fatalf("expected empty-key error, got %v", err)
			}
		})
	}
}

func Test_where_marshalError(t *testing.T) {
	tests := []struct {
		name       string
		build      func() whereReady
		wantParams int
	}{
		{
			"value",
			func() whereReady { return Where().Key("x").Equals().Value(math.Inf(1)) },
			0,
		},
		{
			"values",
			func() whereReady { return Where().Key("x").In().Values("ok", math.NaN()) },
			1,
		},
		{
			"expression_then_and",
			// The inner marshal error must survive the outer And/Key/Equals/Value
			// chain instead of being clobbered back to nil.
			func() whereReady {
				return Where().
					Expression(Where().Key("x").Equals().Value(math.Inf(1))).
					And().Key("y").Equals().Value(2)
			},
			0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, params, err := tt.build().statement()
			if err == nil {
				t.Fatal("expected marshal error, got nil")
			}
			if len(params) != tt.wantParams {
				t.Fatalf("expected %d params, got %d: %v", tt.wantParams, len(params), params)
			}
		})
	}
}

func Test_where_statements(t *testing.T) {
	tests := []struct {
		name       string
		build      func() whereReady
		wantQuery  string
		wantParams []any
	}{
		{
			"noop",
			func() whereReady { return Where().Noop() },
			`1=1`,
			nil,
		},
		{
			"equals_and_equals",
			func() whereReady { return Where().Key("x").Equals().Value(2).And().Key("y").Equals().Value(3) },
			`"object"->'x'=$1 AND "object"->'y'=$2`,
			[]any{"2", "3"},
		},
		{
			"is_null_and_is_null",
			func() whereReady {
				return Where().Key("completed_at").IsNull().
					And().Key("abandoned_at").IsNull().
					OrderByUpdatedAtAsc().LimitPerShard(10)
			},
			`"object"->'completed_at' IS NULL AND "object"->'abandoned_at' IS NULL ORDER BY "updated_at" ASC LIMIT 10`,
			nil,
		},
		{
			"is_not_null_nested",
			func() whereReady { return Where().Key("management.managing_entity").IsNotNull() },
			`"object"->'management'->'managing_entity' IS NOT NULL`,
			nil,
		},
		{
			"equals_explicit_json_null",
			func() whereReady { return Where().Key("deleted_at").Equals().Value(nil) },
			`"object"->'deleted_at'=$1`,
			[]any{"null"},
		},
		{
			"in_values",
			func() whereReady { return Where().Key("x").In().Values("a", 5) },
			`"object"->'x' IN ($1,$2)`,
			[]any{`"a"`, "5"},
		},
		{
			"not_in_values",
			func() whereReady { return Where().Key("x").NotIn().Values(1) },
			`"object"->'x' NOT IN ($1)`,
			[]any{"1"},
		},
		{
			"like",
			func() whereReady { return Where().Key("x").Like().Value("a%") },
			`"object"->'x' LIKE $1`,
			[]any{`"a%"`},
		},
		{
			"search",
			func() whereReady { return Where().Search("hello world") },
			`"text_search" @@ plainto_tsquery('english', $1)`,
			[]any{"hello world"},
		},
		{
			"created_by_or_updated_by",
			func() whereReady { return Where().CreatedBy("alice").Or().UpdatedBy("bob") },
			`"created_by" = $1 OR "updated_by" = $2`,
			[]any{"alice", "bob"},
		},
		{
			"order_limit_offset",
			func() whereReady {
				return Where().Key("x").Equals().Value(1).OrderByAsc("y").LimitPerShard(10).Offset(5)
			},
			`"object"->'x'=$1 ORDER BY "object"->'y' ASC LIMIT 10 OFFSET 5`,
			[]any{"1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, params, err := tt.build().statement()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if query != tt.wantQuery {
				t.Fatalf("query mismatch:\n got %q\nwant %q", query, tt.wantQuery)
			}
			if len(params) != 0 || len(tt.wantParams) != 0 {
				if !reflect.DeepEqual(params, tt.wantParams) {
					t.Fatalf("params mismatch:\n got %#v\nwant %#v", params, tt.wantParams)
				}
			}
		})
	}
}

func Test_where_separatesPredicateFromOrderLimitAndOffset(t *testing.T) {
	where := Where().
		Key("x").Equals().Value("good").
		Or().CreatedBy("operator").
		OrderByUpdatedAtAsc().
		LimitPerShard(10).
		Offset(5)

	predicate, tail, params, err := where.statementParts()
	if err != nil {
		t.Fatalf("statementParts() failed: %v", err)
	}
	if predicate != `"object"->'x'=$1 OR "created_by" = $2` {
		t.Fatalf("unexpected predicate: %q", predicate)
	}
	if tail != `ORDER BY "updated_at" ASC LIMIT 10 OFFSET 5` {
		t.Fatalf("unexpected statement tail: %q", tail)
	}
	if !reflect.DeepEqual(params, []any{`"good"`, "operator"}) {
		t.Fatalf("unexpected params: %#v", params)
	}

	filtered, filteredParams, err := runtimeObjectWhereStatement(where)
	if err != nil {
		t.Fatalf("runtimeObjectWhereStatement() failed: %v", err)
	}
	want := `"object" IS NOT NULL AND ("object"->'x'=$1 OR "created_by" = $2) ORDER BY "updated_at" ASC LIMIT 10 OFFSET 5`
	if filtered != want {
		t.Fatalf("unexpected filtered statement:\n got %q\nwant %q", filtered, want)
	}
	if !reflect.DeepEqual(filteredParams, params) {
		t.Fatalf("filtered params mismatch:\n got %#v\nwant %#v", filteredParams, params)
	}
}

// Test_sanitizeSearchText pins that search text is passed through as data. Most
// cases below are identities, and that is the assertion: the predecessor of this
// function built tsquery syntax in Go, so every metacharacter here was a
// PostgreSQL 42601 syntax error that failed the whole statement. The non-identity
// cases are the hardening — bytes Postgres cannot hold in a text parameter, and
// the operand-length limit that raises the same error class from inside
// plainto_tsquery.
func Test_sanitizeSearchText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Ordinary text is untouched: no collapsing, no trimming, no joining.
		{"single_word", "hello", "hello"},
		{"two_words", "hello world", "hello world"},
		{"keeps_whitespace_runs", "hello \t\n  world", "hello \t\n  world"},
		{"keeps_surrounding_whitespace", "  a b  ", "  a b  "},
		{"unicode_letters", "日本語", "日本語"},

		// The reported crashes: each of these is now data, not syntax.
		{"apostrophe", "O'Brien", "O'Brien"},
		{"leading_apostrophe", "'tis", "'tis"},
		{"single_quote_term", "'", "'"},
		{"trailing_ampersand", "hello &", "hello &"},
		{"unbalanced_close_paren", "hello )", "hello )"},
		{"unbalanced_open_paren", "( hello", "( hello"},
		{"lone_backslash", `\`, `\`},
		{"trailing_backslash", `C:\path\`, `C:\path\`},
		{"colon_after_word", "invoice: 1234", "invoice: 1234"},

		// tsquery operators are no longer honoured, but they are not errors.
		{"negation_operator", "!spam", "!spam"},
		{"prefix_operator", "wor:*", "wor:*"},
		{"phrase_operator", "a <-> b", "a <-> b"},
		{"or_operator", "cat|dog", "cat|dog"},
		{"spaced_or_operator", "a | b", "a | b"},
		{"weight_label", "fat:C", "fat:C"},

		// Degenerate input.
		{"pure_punctuation", "!@#$%^&*()", "!@#$%^&*()"},
		{"mixed_valid_and_punctuation", "hello &&& world", "hello &&& world"},
		{"empty", "", ""},
		{"whitespace_only", "   ", "   "},

		// Bytes Postgres cannot hold in a text parameter.
		{"nul_becomes_space", "a\x00b", "a b"},
		{"invalid_utf8_becomes_space", "caf\xe9", "caf "},

		// The length cap that keeps "value is too big in tsquery" out of reach.
		// "日" is three bytes and 65536 is not a multiple of three, so the cut
		// lands mid-rune and must step back to 65535 — 21845 whole runes.
		{"at_byte_cap_unchanged", strings.Repeat("a", maxSearchTextBytes), strings.Repeat("a", maxSearchTextBytes)},
		{"over_byte_cap_truncated", strings.Repeat("a", maxSearchTextBytes+953), strings.Repeat("a", maxSearchTextBytes)},
		{"over_byte_cap_cuts_on_rune_boundary", strings.Repeat("日", 30000), strings.Repeat("日", 21845)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeSearchText(tt.input)
			if got != tt.want {
				t.Fatalf("sanitizeSearchText(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if len(got) > maxSearchTextBytes {
				t.Fatalf("sanitizeSearchText(%q) returned %d bytes, want at most %d", tt.input, len(got), maxSearchTextBytes)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("sanitizeSearchText(%q) = %q, which is not valid UTF-8", tt.input, got)
			}
			if strings.ContainsRune(got, 0) {
				t.Fatalf("sanitizeSearchText(%q) = %q, which contains a NUL byte", tt.input, got)
			}
		})
	}
}

// Test_where_search_bindsTextAsData pins the contract the old to_tsquery path
// violated: the statement is fixed and the caller's text reaches the parameter
// unaltered. Nothing about the SQL may depend on the input.
func Test_where_search_bindsTextAsData(t *testing.T) {
	const wantQuery = `"text_search" @@ plainto_tsquery('english', $1)`

	inputs := []struct {
		name  string
		input string
	}{
		{"two_words", "hello world"},
		{"apostrophe", "O'Brien"},
		{"leading_apostrophe", "'tis"},
		{"single_quote_term", "'"},
		{"trailing_ampersand", "hello &"},
		{"unbalanced_close_paren", "hello )"},
		{"unbalanced_open_paren", "( hello"},
		{"negation_operator", "!spam"},
		{"prefix_operator", "wor:*"},
		{"phrase_operator", "a <-> b"},
		{"lone_backslash", `\`},
		{"trailing_backslash", `C:\path\`},
		{"pure_punctuation", "!@#$%^&*()"},
		{"mixed_valid_and_punctuation", "hello &&& world"},
		{"empty", ""},
		{"whitespace_only", "   "},
	}

	for _, tt := range inputs {
		t.Run(tt.name, func(t *testing.T) {
			query, params, err := Where().Search(tt.input).statement()
			if err != nil {
				t.Fatalf("Search(%q) returned error: %v", tt.input, err)
			}
			if query != wantQuery {
				t.Fatalf("Search(%q) query mismatch:\n got %q\nwant %q", tt.input, query, wantQuery)
			}
			if !reflect.DeepEqual(params, []any{tt.input}) {
				t.Fatalf("Search(%q) params mismatch:\n got %#v\nwant %#v", tt.input, params, []any{tt.input})
			}
			assertPlaceholdersConsistent(t, query, params)
		})
	}
}

// Test_where_search_doesNotBuildTSQuery guards the two ways this fix can be
// undone: routing back through to_tsquery, which parses its argument as tsquery
// syntax, or interpolating the caller's text into the statement instead of
// binding it. Note that strings.Contains(query, "to_tsquery") is true for
// plainto_tsquery — the "@@ " prefix is what makes the check meaningful.
func Test_where_search_doesNotBuildTSQuery(t *testing.T) {
	const probe = `x') OR '1'='1`

	query, params, err := Where().Search(probe).statement()
	if err != nil {
		t.Fatalf("Search(%q) returned error: %v", probe, err)
	}
	if strings.Contains(query, "@@ to_tsquery(") {
		t.Fatalf("Search must not call to_tsquery, which parses tsquery syntax: %q", query)
	}
	if !strings.Contains(query, "@@ plainto_tsquery('english', $1)") {
		t.Fatalf("Search must bind through plainto_tsquery: %q", query)
	}
	if strings.Contains(query, probe) {
		t.Fatalf("search text leaked into the statement: %q", query)
	}
	if !reflect.DeepEqual(params, []any{probe}) {
		t.Fatalf("search text must be bound verbatim: %#v", params)
	}
}

// FuzzSearchBindsTextAsData is the only mechanism that will find the next edge
// case: for any input the statement must not vary and the bound value must
// always be something Postgres can accept as a text parameter.
func FuzzSearchBindsTextAsData(f *testing.F) {
	for _, seed := range []string{"O'Brien", "hello &", "bar )", `C:\path\`, "'", "!spam", "wor:*", "", "   ", "a\x00b"} {
		f.Add(seed)
	}

	const wantQuery = `"text_search" @@ plainto_tsquery('english', $1)`

	f.Fuzz(func(t *testing.T, text string) {
		query, params, err := Where().Search(text).statement()
		if err != nil {
			t.Fatalf("Search(%q) returned error: %v", text, err)
		}
		if query != wantQuery {
			t.Fatalf("statement depends on input %q: %q", text, query)
		}
		if len(params) != 1 {
			t.Fatalf("Search(%q) bound %d params, want 1", text, len(params))
		}
		got, ok := params[0].(string)
		if !ok {
			t.Fatalf("Search(%q) bound %T, want string", text, params[0])
		}
		if len(got) > maxSearchTextBytes {
			t.Fatalf("Search(%q) bound %d bytes, want at most %d", text, len(got), maxSearchTextBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Search(%q) bound %q, which is not valid UTF-8", text, got)
		}
		if strings.ContainsRune(got, 0) {
			t.Fatalf("Search(%q) bound %q, which contains a NUL byte", text, got)
		}
	})
}

var testPlaceholderPattern = regexp.MustCompile(`\$(\d+)`)

// assertPlaceholdersConsistent verifies the contract between a generated SQL
// statement and its parameter slice: every parameter is referenced by at
// least one placeholder, and no placeholder references a missing parameter.
// A violation surfaces in Postgres as 42P18 ("could not determine data type
// of parameter $n") or a bind-count mismatch.
func assertPlaceholdersConsistent(t *testing.T, sql string, params []any) {
	t.Helper()
	referenced := make(map[int]bool)
	for _, m := range testPlaceholderPattern.FindAllStringSubmatch(sql, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unparsable placeholder %q in SQL: %s", m[0], sql)
		}
		if n < 1 || n > len(params) {
			t.Errorf("placeholder $%d out of range (have %d params) in SQL: %s", n, len(params), sql)
		}
		referenced[n] = true
	}
	for i := 1; i <= len(params); i++ {
		if !referenced[i] {
			t.Errorf("parameter $%d is never referenced (Postgres 42P18) in SQL: %s", i, sql)
		}
	}
}

func Test_where_expression_placeholder_renumbering(t *testing.T) {

	manyValues := func(n int) []any {
		vs := make([]any, n)
		for i := range vs {
			vs[i] = "v" + strconv.Itoa(i)
		}
		return vs
	}

	tests := []struct {
		name        string
		where       whereExpectingLogicalOperator
		expectSQL   string
		expectCount int
	}{
		{
			// Search binds $1, then a 2-param OR expression is embedded.
			name: "expression_after_one_param",
			where: Where().Noop().
				And().Search("niralee").
				And().Expression(
				Where().
					Key("grants.allow_private_investments").Equals().Value(true).
					Or().
					Key("requested_products.private_investments").Equals().Value(true),
			).
				And().Key("management.managing_entity").Equals().Value("some-entity"),
			expectSQL: `1=1 AND "text_search" @@ plainto_tsquery('english', $1) AND ` +
				`("object"->'grants'->'allow_private_investments'=$2 OR "object"->'requested_products'->'private_investments'=$3)` +
				` AND "object"->'management'->'managing_entity'=$4`,
			expectCount: 4,
		},
		{
			// An expression embedded as the first statement, then a second
			// expression that itself contains a nested expression (cursor
			// pagination shape: created_at < a OR (created_at = a AND id < b)).
			name: "nested_expression_after_expression",
			where: Where().Expression(
				Where().
					Key("email.to").Equals().Value("e1").
					Or().
					Key("push.to").Equals().Value("e1"),
			).
				And().Expression(
				Where().
					Key("created_at").LessThan().Value("at").
					Or().
					Expression(
						Where().
							Key("created_at").Equals().Value("at").
							And().
							Key("message_id").LessThan().Value("mid"),
					),
			),
			expectSQL: `("object"->'email'->'to'=$1 OR "object"->'push'->'to'=$2) AND ` +
				`("object"->'created_at'<$3 OR ("object"->'created_at'=$4 AND "object"->'message_id'<$5))`,
			expectCount: 5,
		},
		{
			// IN-list before an expression holding two IN-lists over the
			// same values (find-offers shape).
			name: "expression_with_in_lists_after_one_param",
			where: Where().Noop().
				And().Key("user_id").In().Values("u1").
				And().Expression(
				Where().
					Key("user_id").In().Values("a", "b").
					Or().
					Key("ad_user_id").In().Values("a", "b"),
			),
			expectSQL: `1=1 AND "object"->'user_id' IN ($1) AND ` +
				`("object"->'user_id' IN ($2,$3) OR "object"->'ad_user_id' IN ($4,$5))`,
			expectCount: 5,
		},
		{
			// Ten params on each side: renumbering $1 must not also rewrite
			// the "$1" prefix of "$10".
			name: "expression_with_double_digit_placeholders",
			where: Where().Noop().
				And().Key("a").In().Values(manyValues(10)...).
				And().Expression(
				Where().
					Key("b").In().Values(manyValues(10)...),
			),
			expectSQL: `1=1 AND "object"->'a' IN ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) AND ` +
				`("object"->'b' IN ($11,$12,$13,$14,$15,$16,$17,$18,$19,$20))`,
			expectCount: 20,
		},
		{
			// Two consecutive 2-param expressions from a zero-param base:
			// already worked before the fix and must keep working.
			name: "consecutive_expressions_from_zero_params",
			where: Where().Noop().
				And().Expression(
				Where().
					Key("grants.allow_investments").Equals().Value(true).
					Or().
					Key("requested_products.notes_investments").Equals().Value(true),
			).
				And().Expression(
				Where().
					Key("grants.allow_private_investments").Equals().Value(true).
					Or().
					Key("requested_products.private_investments").Equals().Value(true),
			),
			expectSQL: `1=1 AND ` +
				`("object"->'grants'->'allow_investments'=$1 OR "object"->'requested_products'->'notes_investments'=$2) AND ` +
				`("object"->'grants'->'allow_private_investments'=$3 OR "object"->'requested_products'->'private_investments'=$4)`,
			expectCount: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, params, err := tt.where.statement()
			if err != nil {
				t.Fatalf("statement() failed: %v", err)
			}
			if len(params) != tt.expectCount {
				t.Errorf("expected %d params, got %d", tt.expectCount, len(params))
			}
			if sql != tt.expectSQL {
				t.Errorf("unexpected SQL\n want: %s\n  got: %s", tt.expectSQL, sql)
			}
			assertPlaceholdersConsistent(t, sql, params)
		})
	}
}
