package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func (tos TenantObjectSet[objT, idT, shardKeyT]) Process(ctx convCtx.Context, where whereReady, process func(ctx convCtx.Context, obj objT) error, shardKeys ...shardKeyT) (count int, err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	var dbs []*sql.DB
	dbs, err = dbsForShardKeys(tos.vault, tos.tenant, shardKeys...)
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
		rows, err = db.Query(`SELECT "object" FROM "`+tos.table.RuntimeTableName+`" WHERE `+statement, params...)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}
		defer rows.Close() // see select.go's SelectAll for why deferring inside this loop is safe

		for rows.Next() {

			var (
				bytes []byte
				obj   objT
			)

			err = rows.Scan(&bytes)
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

			err = process(ctx, obj)
			if err != nil {
				return
			}

			count++
		}
		if err = rows.Err(); err != nil {
			// count already reflects every row successfully processed
			// before the driver reported this iteration error.
			return
		}

	}

	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) ProcessWithMetadata(ctx convCtx.Context, where whereReady, process func(ctx convCtx.Context, obj ObjectWithMetadata[objT]) error, shardKeys ...shardKeyT) (count int, err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	var dbs []*sql.DB
	dbs, err = dbsForShardKeys(tos.vault, tos.tenant, shardKeys...)
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
		defer rows.Close() // see select.go's SelectAll for why deferring inside this loop is safe

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

			err = process(ctx, ObjectWithMetadata[objT]{obj, md})
			if err != nil {
				return
			}

			count++
		}
		if err = rows.Err(); err != nil {
			// count already reflects every row successfully processed
			// before the driver reported this iteration error.
			return
		}

	}

	return
}
