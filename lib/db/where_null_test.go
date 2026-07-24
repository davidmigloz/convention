package db

import (
	"testing"
)

func TestWhereNullPredicates(t *testing.T) {
	t.Run("is null", func(t *testing.T) {
		query, params, err := Where().
			Key("completed_at").IsNull().
			And().Key("abandoned_at").IsNull().
			OrderByUpdatedAtAsc().
			LimitPerShard(10).
			statement()
		if err != nil {
			t.Fatal(err)
		}
		const want = `"object"->'completed_at' IS NULL AND "object"->'abandoned_at' IS NULL ORDER BY "updated_at" ASC LIMIT 10`
		if query != want {
			t.Fatalf("query = %q, want %q", query, want)
		}
		if len(params) != 0 {
			t.Fatalf("params = %#v, want no parameters", params)
		}
	})

	t.Run("is not null", func(t *testing.T) {
		query, params, err := Where().
			Key("management.managing_entity").IsNotNull().
			statement()
		if err != nil {
			t.Fatal(err)
		}
		const want = `"object"->'management'->'managing_entity' IS NOT NULL`
		if query != want {
			t.Fatalf("query = %q, want %q", query, want)
		}
		if len(params) != 0 {
			t.Fatalf("params = %#v, want no parameters", params)
		}
	})
}
