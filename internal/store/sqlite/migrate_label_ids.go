// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// labelIDsMigration (version 5) gives every label that predates label ids
// (migration 4) a random id, then makes ids unique. It is a Go migration
// because portable SQL has no random-id function (see PORTABILITY.md).
var labelIDsMigration = goose.NewGoMigration(5,
	&goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT module_id, name FROM labels WHERE id = ''`)
		if err != nil {
			return err
		}
		type key struct{ module, name string }
		var keys []key
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.module, &k.name); err != nil {
				_ = rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, `UPDATE labels SET id = ? WHERE module_id = ? AND name = ?`,
				newID(), k.module, k.name); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `CREATE UNIQUE INDEX labels_id_idx ON labels (id)`)
		return err
	}},
	&goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP INDEX labels_id_idx`)
		return err
	}},
)
