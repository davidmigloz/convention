package db

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
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
			`"text_search" @@ to_tsquery('english', $1)`,
			[]any{"hello & world"},
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

func Test_toTSQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"single_word", "hello", "hello"},
		{"two_words", "hello world", "hello & world"},
		{"collapses_whitespace", "hello \t\n  world", "hello & world"},
		{"trims_surrounding_whitespace", "  a b  ", "a & b"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toTSQuery(tt.input); got != tt.want {
				t.Fatalf("toTSQuery(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
