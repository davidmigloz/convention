package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func (tos TenantObjectSet[objT, idT, shardKeyT]) SelectAll(ctx convCtx.Context) (obs []objT, err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	dbs, err := DBs(tos.vault, tos.tenant)
	if err != nil {
		return
	}

	for _, db := range dbs {

		var rows *sql.Rows
		rows, err = db.Query(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "` + tos.table.RuntimeTableName + `" WHERE "object" IS NOT NULL`)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}

		// rows.Close is deferred rather than closed at the bottom of this
		// shard's iteration: defers are function-scoped, not
		// loop-iteration-scoped, so they accumulate across every shard in
		// this range and only actually run when SelectAll returns. That is
		// safe and correct here: (a) a happy-path shard's rows already
		// auto-close the moment Next() reports exhaustion, releasing the
		// connection immediately — the later deferred Close on those same,
		// already-closed rows is a documented no-op; (b) an early error
		// return runs every accumulated defer, so whichever single cursor
		// is still open (at most one — earlier shards already exhausted
		// theirs) gets closed.
		defer rows.Close()

		for rows.Next() {

			var (
				bytes []byte
				obj   objT
				md    Metadata
			)

			err = rows.Scan(&bytes, &md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
			if err != nil {
				return
			}
			if bytes == nil {
				continue
			}

			err = json.Unmarshal(bytes, &obj)
			if err != nil {
				return
			}

			for _, compute := range tos.compute {
				err = compute(ctx, md, &obj)
				if err != nil {
					return
				}
			}

			obs = append(obs, obj)
		}
		if err = rows.Err(); err != nil {
			return
		}

	}

	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) SelectAllWithMetadata(ctx convCtx.Context) (obs ListWithMetadata[objT], err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	dbs, err := DBs(tos.vault, tos.tenant)
	if err != nil {
		return
	}

	for _, db := range dbs {

		var rows *sql.Rows
		rows, err = db.Query(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "` + tos.table.RuntimeTableName + `" WHERE "object" IS NOT NULL`)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}
		defer rows.Close() // see SelectAll above for why deferring inside this loop is safe

		for rows.Next() {

			var (
				bytes []byte
				obj   objT
				md    Metadata
			)

			err = rows.Scan(&bytes, &md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
			if err != nil {
				return
			}
			if bytes == nil {
				continue
			}

			err = json.Unmarshal(bytes, &obj)
			if err != nil {
				return
			}

			for _, compute := range tos.compute {
				err = compute(ctx, md, &obj)
				if err != nil {
					return
				}
			}

			obs = append(obs, ObjectWithMetadata[objT]{obj, md})
		}
		if err = rows.Err(); err != nil {
			return
		}

	}

	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) SelectByID(ctx convCtx.Context, id idT, shardKeys ...shardKeyT) (obj *objT, err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	dbs, err := dbsForShardKeys(tos.vault, tos.tenant, shardKeys...)
	if err != nil {
		return
	}

	for _, db := range dbs {

		var (
			bytes []byte
			md    Metadata
		)

		err = db.
			QueryRow(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "`+tos.table.RuntimeTableName+`" WHERE id=$1 AND "object" IS NOT NULL`, id).
			Scan(&bytes, &md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}
		if bytes == nil {
			continue
		}

		obj = new(objT)
		err = json.Unmarshal(bytes, obj)
		if err != nil {
			return
		}

		for _, compute := range tos.compute {
			err = compute(ctx, md, obj)
			if err != nil {
				return
			}
		}

	}
	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) SelectByIDWithMetadata(ctx convCtx.Context, id idT, shardKeys ...shardKeyT) (obj *ObjectWithMetadata[objT], err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	dbs, err := dbsForShardKeys(tos.vault, tos.tenant, shardKeys...)
	if err != nil {
		return
	}

	for _, db := range dbs {

		var (
			bytes []byte
			o     objT
			md    Metadata
		)

		err = db.
			QueryRow(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "`+tos.table.RuntimeTableName+`" WHERE id=$1 AND "object" IS NOT NULL`, id).
			Scan(&bytes, &md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}
		if bytes == nil {
			continue
		}

		err = json.Unmarshal(bytes, &o)
		if err != nil {
			return
		}

		for _, compute := range tos.compute {
			err = compute(ctx, md, &o)
			if err != nil {
				return
			}
		}

		obj = &ObjectWithMetadata[objT]{o, md}
	}
	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) Select(ctx convCtx.Context, where whereReady, shardKeys ...shardKeyT) (obs []objT, err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	dbs, err := dbsForShardKeys(tos.vault, tos.tenant, shardKeys...)
	if err != nil {
		return
	}

	statement, params, err := runtimeObjectWhereStatement(where)
	if err != nil {
		err = fmt.Errorf("error building where statement: %w", err)
		return
	}

	for _, db := range dbs {

		var rows *sql.Rows
		rows, err = db.Query(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "`+tos.table.RuntimeTableName+`" WHERE `+statement, params...)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}
		defer rows.Close() // see SelectAll above for why deferring inside this loop is safe

		for rows.Next() {

			var (
				bytes []byte
				obj   objT
				md    Metadata
			)

			err = rows.Scan(&bytes, &md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
			if err != nil {
				return
			}
			if bytes == nil {
				continue
			}

			err = json.Unmarshal(bytes, &obj)
			if err != nil {
				return
			}

			for _, compute := range tos.compute {
				err = compute(ctx, md, &obj)
				if err != nil {
					return
				}
			}

			obs = append(obs, obj)
		}
		if err = rows.Err(); err != nil {
			return
		}

	}

	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) SelectWithMetadata(ctx convCtx.Context, where whereReady, shardKeys ...shardKeyT) (obs ListWithMetadata[objT], err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	dbs, err := dbsForShardKeys(tos.vault, tos.tenant, shardKeys...)
	if err != nil {
		return
	}

	statement, params, err := runtimeObjectWhereStatement(where)
	if err != nil {
		err = fmt.Errorf("error building where statement: %w", err)
		return
	}

	for _, db := range dbs {

		var rows *sql.Rows
		rows, err = db.Query(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "`+tos.table.RuntimeTableName+`" WHERE `+statement, params...)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}
		defer rows.Close() // see SelectAll above for why deferring inside this loop is safe

		for rows.Next() {

			var (
				bytes []byte
				obj   objT
				md    Metadata
			)

			err = rows.Scan(&bytes, &md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
			if err != nil {
				return
			}
			if bytes == nil {
				continue
			}

			err = json.Unmarshal(bytes, &obj)
			if err != nil {
				return
			}

			for _, compute := range tos.compute {
				err = compute(ctx, md, &obj)
				if err != nil {
					return
				}
			}

			obs = append(obs, ObjectWithMetadata[objT]{obj, md})
		}
		if err = rows.Err(); err != nil {
			return
		}

	}

	return
}

func runtimeObjectWhereStatement(where whereReady) (statement string, params []any, err error) {
	predicate, tail, params, err := where.statementParts()
	if err != nil {
		return "", params, err
	}

	statement = `"object" IS NOT NULL AND (` + predicate + `)`
	if tail != "" {
		statement += " " + tail
	}
	return statement, params, nil
}
