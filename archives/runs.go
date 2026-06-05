package archives

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nyaruka/archiver/v26/runtime"
	"github.com/nyaruka/gocommon/dates"
	"github.com/vinovest/sqlx"
)

const (
	RunStatusActive      = "A"
	RunStatusWaiting     = "W"
	RunStatusCompleted   = "C"
	RunStatusExpired     = "X"
	RunStatusInterrupted = "I"
	RunStatusFailed      = "F"
)

const sqlLookupRuns = `
SELECT rec.uuid, row_to_json(rec)
FROM (
	SELECT
		fr.id,
		fr.uuid,
		row_to_json(flow_struct) AS flow,
		row_to_json(contact_struct) AS contact,
		fr.responded,
		(SELECT CASE
			WHEN (fr.path_nodes IS NOT NULL AND fr.path_times IS NOT NULL)
			THEN (
				SELECT coalesce(jsonb_agg(path_data), '[]'::jsonb)
				FROM (
					SELECT node, time
					FROM unnest(fr.path_nodes::text[] , fr.path_times::timestamptz[]) x(node, time) LIMIT 500)
					AS path_data)
			ELSE '[]'::jsonb
		END AS path),
		(SELECT coalesce(jsonb_object_agg(values_data.key, values_data.value), '{}'::jsonb) FROM (
			SELECT key, jsonb_build_object('name', value -> 'name', 'value', value -> 'value', 'input', value -> 'input', 'time', (value -> 'created_on')::text::timestamptz, 'category', value -> 'category', 'node', value -> 'node_uuid') as value
			FROM jsonb_each(fr.results::jsonb)) AS values_data
		) AS values,
		fr.created_on,
		fr.modified_on,
		fr.exited_on,
		CASE
			WHEN status = 'C' THEN 'completed'
			WHEN status = 'I' THEN 'interrupted'
			WHEN status = 'X' THEN 'expired'
			WHEN status = 'F' THEN 'failed'
			ELSE NULL
		END AS exit_type

	FROM flows_flowrun fr
	JOIN LATERAL (SELECT uuid, name FROM flows_flow WHERE flows_flow.id = fr.flow_id) AS flow_struct ON True
	JOIN LATERAL (SELECT uuid, name FROM contacts_contact cc WHERE cc.id = fr.contact_id) AS contact_struct ON True
	WHERE fr.org_id = $1 AND fr.modified_on >= $2 AND fr.modified_on < $3
	ORDER BY fr.modified_on ASC, id ASC
) as rec;`

// writeRunRecords writes the runs in the archive's date range to the passed in writer
func writeRunRecords(ctx context.Context, db *sqlx.DB, archive *Archive, writer *bufio.Writer) (int, error) {
	var rows *sqlx.Rows
	rows, err := db.QueryxContext(ctx, sqlLookupRuns, archive.Org.ID, archive.StartDate, archive.endDate())
	if err != nil {
		return 0, fmt.Errorf("error querying run records for org: %d: %w", archive.Org.ID, err)
	}
	defer rows.Close()

	recordCount := 0

	for rows.Next() {
		var runUUID string
		var record string

		if err := rows.Scan(&runUUID, &record); err != nil {
			return 0, fmt.Errorf("error scanning run record for org: %d: %w", archive.Org.ID, err)
		}

		writer.WriteString(record)
		writer.WriteString("\n")
		recordCount++
	}

	return recordCount, nil
}

const sqlSelectOrgRunsInRange = `
   SELECT fr.id
     FROM flows_flowrun fr
LEFT JOIN contacts_contact cc ON cc.id = fr.contact_id
    WHERE fr.org_id = $1 AND fr.modified_on >= $2 AND fr.modified_on < $3
 ORDER BY fr.modified_on ASC, fr.id ASC`

const sqlDeleteRuns = `
DELETE FROM flows_flowrun WHERE id IN(?)`

// DeleteArchivedRuns takes the passed in archive, verifies the S3 file is still present (and correct), then selects
// all the runs in the archive date range, and if equal or fewer than the number archived, deletes them 100 at a time
//
// Upon completion it updates the needs_deletion flag on the archive
func DeleteArchivedRuns(ctx context.Context, rt *runtime.Runtime, archive *Archive) error {
	outer, cancel := context.WithTimeout(ctx, time.Hour*3)
	defer cancel()

	start := dates.Now()
	log := slog.With(
		"id", archive.ID,
		"org_id", archive.OrgID,
		"start_date", archive.StartDate,
		"end_date", archive.endDate(),
		"archive_type", archive.ArchiveType,
		"total_count", archive.RecordCount,
	)
	log.Info("deleting runs")

	// make sure our archive file is still correct on S3 before we delete anything
	if err := verifyUploadedArchive(outer, rt, archive); err != nil {
		return err
	}

	// ok, archive file looks good, let's build up our list of run ids, this may be big but we are int64s so shouldn't be too big
	rows, err := rt.DB.QueryxContext(outer, sqlSelectOrgRunsInRange, archive.OrgID, archive.StartDate, archive.endDate())
	if err != nil {
		return err
	}
	defer rows.Close()

	var runID int64
	runIDs := make([]int64, 0, archive.RecordCount)
	for rows.Next() {
		if err := rows.Scan(&runID); err != nil {
			return err
		}
		runIDs = append(runIDs, runID)
	}
	rows.Close()

	log.Debug("found runs", "run_count", len(runIDs))

	// verify we don't see more runs than there are in our archive (fewer is ok)
	if len(runIDs) > archive.RecordCount {
		return fmt.Errorf("more runs in the database: %d than in archive: %d", len(runIDs), archive.RecordCount)
	}

	// ok, delete our runs in batches, we do this in transactions as it spans a few different queries
	for idBatch := range slices.Chunk(runIDs, deleteTransactionSize) {
		// no single batch should take more than a few minutes
		ctx, cancel := context.WithTimeout(ctx, time.Minute*15)
		defer cancel()

		start := dates.Now()

		// start our transaction
		tx, err := rt.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}

		// delete our runs
		if err := executeInQuery(ctx, tx, sqlDeleteRuns, idBatch); err != nil {
			return fmt.Errorf("error deleting runs: %w", err)
		}

		// commit our transaction
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("error committing run delete transaction: %w", err)
		}

		log.Debug("deleted batch of runs", "elapsed", dates.Since(start), "count", len(idBatch))

		cancel()
	}

	slog.Info("completed deleting runs", "elapsed", dates.Since(start))

	return nil
}

const selectOldOrgFlowStarts = `
 SELECT id
   FROM flows_flowstart s
  WHERE s.org_id = $1 AND s.created_on < $2 AND NOT EXISTS (SELECT 1 FROM flows_flowrun WHERE start_id = s.id)
  LIMIT 1000000;`

// DeleteFlowStarts deletes all starts older than the org's retention period which have no associated runs
func DeleteFlowStarts(ctx context.Context, rt *runtime.Runtime, now time.Time, org Org) error {
	return deleteOrphans(ctx, rt, now, org, orphanDeletion{
		what:      "start",
		selectSQL: selectOldOrgFlowStarts,
		childSQL: []string{
			`DELETE from flows_flowstart_contacts WHERE flowstart_id = $1`,
			`DELETE from flows_flowstart_groups WHERE flowstart_id = $1`,
			`DELETE from flows_flowstartcount WHERE start_id = $1`,
		},
		parentSQL: `DELETE from flows_flowstart WHERE id = $1`,
	})
}
