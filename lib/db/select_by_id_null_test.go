package db_test

import (
	"testing"
)

func Test_SelectByID_treats_SQL_NULL_object_as_absent(t *testing.T) {
	db, ctx := setupLockCleanupTest(t)
	id := LockCleanupID("select-by-id-sql-null")
	insertLockCleanupRuntimeRow(t, db, id, nil)

	obj, err := lockCleanupDB.Tenant("test").SelectByID(ctx, id)

	if err != nil {
		t.Errorf("SelectByID returned an error for an absent object: %v", err)
	}
	if obj != nil {
		t.Errorf("SelectByID returned object %#v for an SQL-NULL row", obj)
	}
}

func Test_SelectByIDWithMetadata_treats_SQL_NULL_object_as_absent(t *testing.T) {
	db, ctx := setupLockCleanupTest(t)
	id := LockCleanupID("select-by-id-with-metadata-sql-null")
	insertLockCleanupRuntimeRow(t, db, id, nil)

	obj, err := lockCleanupDB.Tenant("test").SelectByIDWithMetadata(ctx, id)

	if err != nil {
		t.Errorf("SelectByIDWithMetadata returned an error for an absent object: %v", err)
	}
	if obj != nil {
		t.Errorf(
			"SelectByIDWithMetadata returned object %#v for an SQL-NULL row",
			obj,
		)
	}
}
