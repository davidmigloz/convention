package db

import (
	"strings"
	"testing"
)

type idxTestObj struct{}

func (idxTestObj) DBKey() Key[string, string] { return Key[string, string]{} }

// Test_WithIndexes_recordsPassedKeys is the red-first guard: WithIndexes must
// record exactly the field names it is given. Before the fix it discarded its
// args and recorded the object type name instead.
func Test_WithIndexes_recordsPassedKeys(t *testing.T) {
	osIface := NewObjectSet[idxTestObj, string, string](Vault("test")).
		WithIndexes("foo", "bar")

	os, ok := osIface.(*objectSet[idxTestObj, string, string])
	if !ok {
		t.Fatalf("NewObjectSet did not return *objectSet")
	}

	want := []string{"foo", "bar"}
	if len(os.indexes) != len(want) {
		t.Fatalf("WithIndexes recorded %v, want %v", os.indexes, want)
	}
	for i := range want {
		if os.indexes[i] != want[i] {
			t.Fatalf("WithIndexes recorded %v, want %v", os.indexes, want)
		}
	}
}

func Test_jsonIndexStatement_btreeOnJSONBExpression(t *testing.T) {
	ddl := jsonIndexStatement("entity", "type")
	if !strings.Contains(ddl, `("object"->'type')`) {
		t.Fatalf("expected JSONB expression on the key, got: %s", ddl)
	}
	if !strings.Contains(ddl, "CREATE INDEX IF NOT EXISTS") {
		t.Fatalf("expected idempotent CREATE INDEX, got: %s", ddl)
	}
	if strings.Contains(strings.ToLower(ddl), "using gin") {
		t.Fatalf("expected a btree (no USING gin) index, got: %s", ddl)
	}
}

func Test_jsonIndexStatement_nestedKeyPath(t *testing.T) {
	ddl := jsonIndexStatement("entity", "grants.allow_pro_supply")
	if !strings.Contains(ddl, `("object"->'grants'->'allow_pro_supply')`) {
		t.Fatalf("expected nested JSONB path, got: %s", ddl)
	}
}

func Test_indexName_collisionSafeForShortKeys(t *testing.T) {
	dotted := indexName("entity", "a.b")
	underscored := indexName("entity", "a_b")
	if dotted == underscored {
		t.Fatalf("index names for distinct keys collided: %q == %q", dotted, underscored)
	}
}

func Test_indexName_respects63CharLimit(t *testing.T) {
	long := strings.Repeat("verylongsegment.", 8) + "leaf"
	name := indexName("some_long_runtime_table_name", long)
	if len(name) > 63 {
		t.Fatalf("index name exceeds 63 chars (%d): %q", len(name), name)
	}
	// Even after truncation, two different long keys must stay distinct.
	other := indexName("some_long_runtime_table_name", long+"x")
	if name == other {
		t.Fatalf("truncated long index names collided: %q == %q", name, other)
	}
}
