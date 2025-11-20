package premigrations

import (
	"fmt"

	"github.com/docker/distribution/registry/datastore/migrations"
	migrate "github.com/rubenv/sql-migrate"
)

func init() {
	const numPartitions = 64

	upStatements := make([]string, 0, numPartitions)
	downStatements := make([]string, 0, numPartitions)

	for i := 0; i < numPartitions; i++ {
		partitionName := fmt.Sprintf("partitions.blobs_p_%d", i)
		migrationName := fmt.Sprintf("backfill_blobs_p_%d_id_column", i)

		upStatements = append(upStatements, fmt.Sprintf(`
        INSERT INTO batched_background_migrations ("name", "min_value", "max_value", "batch_size", "status", "job_signature_name", "table_name", "column_name", "batching_strategy")
            VALUES ('%s', 0, 0, 100000, 1, -- active
                'populateBlobsIDColumn', '%s', 'id', 'NullKeySetBatchingStrategy')`,
			migrationName,
			partitionName,
		))

		downStatements = append(downStatements, fmt.Sprintf(`
			DELETE FROM batched_background_migrations WHERE "name" = '%s'`,
			migrationName,
		))
	}

	m := &migrations.Migration{
		Migration: &migrate.Migration{
			Id:   "20251021070732_bbm_backfill_blobs_id_column",
			Up:   upStatements,
			Down: downStatements,
		},
	}

	migrations.AppendPreMigration(m)
}
