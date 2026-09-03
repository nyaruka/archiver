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

const folderDeleted = "D"

const sqlLookupMsgs = `
SELECT rec.visibility, row_to_json(rec) FROM (
	SELECT
		mm.uuid,
		mm.id,
		broadcast_id AS broadcast,
		row_to_json(contact) AS contact,
		CASE WHEN oo.is_anon = FALSE THEN ccu.identity ELSE NULL END AS urn,
		row_to_json(channel) AS channel,
		row_to_json(flow) AS flow,
		mm.ticket_uuid,
		CASE WHEN direction = 'I' THEN 'in' WHEN direction = 'O' THEN 'out' ELSE NULL END AS direction,
		CASE 
			WHEN msg_type = 'T' THEN 'text'
			WHEN msg_type = 'O' THEN 'optin'
			WHEN msg_type = 'V' THEN 'voice'
			ELSE NULL 
		END AS "type",
		CASE 
			WHEN status = 'I' THEN 'initializing'
			WHEN status = 'P' THEN 'queued'
			WHEN status = 'Q' THEN 'queued'
			WHEN status = 'W' THEN 'wired'
			WHEN status = 'D' THEN 'delivered'
			WHEN status = 'H' THEN 'handled'
			WHEN status = 'E' THEN 'errored'
			WHEN status = 'F' THEN 'failed'
			WHEN status = 'S' THEN 'sent'
			WHEN status = 'R' THEN 'read'
			ELSE NULL 
		END AS status,
		CASE WHEN folder = 'A' THEN 'archived' WHEN folder = 'D' THEN 'deleted' ELSE 'visible' END AS visibility,
		text,
		(SELECT coalesce(jsonb_agg(attach_row), '[]'::jsonb) FROM (SELECT attach_data.attachment[1] AS content_type, attach_data.attachment[2] AS url FROM (SELECT regexp_matches(unnest(attachments), '^(.*?):(.*)$') attachment) AS attach_data) AS attach_row) AS attachments,
		labels_agg.data AS labels,
		mm.created_on,
		mm.sent_on,
		mm.modified_on
	FROM msgs_msg mm 
		JOIN orgs_org oo ON mm.org_id = oo.id
		JOIN LATERAL (SELECT uuid, name FROM contacts_contact cc WHERE cc.id = mm.contact_id) AS contact ON True
		LEFT JOIN contacts_contacturn ccu ON mm.contact_urn_id = ccu.id
		LEFT JOIN LATERAL (SELECT uuid, name FROM channels_channel ch WHERE ch.id = mm.channel_id) AS channel ON True
		LEFT JOIN LATERAL (SELECT uuid, name FROM flows_flow f WHERE f.id = mm.flow_id) AS flow ON True
		LEFT JOIN LATERAL (SELECT coalesce(jsonb_agg(label_row), '[]'::jsonb) AS data FROM (SELECT uuid, name FROM msgs_label ml INNER JOIN msgs_msg_labels mml ON ml.id = mml.label_id AND mml.msg_id = mm.id) AS label_row) AS labels_agg ON True

	WHERE mm.org_id = $1 AND mm.created_on >= $2 AND mm.created_on < $3
ORDER BY created_on ASC, id ASC) rec;`

// writeMessageRecords writes the messages in the archive's date range to the passed in writer
func writeMessageRecords(ctx context.Context, db *sqlx.DB, archive *Archive, writer *bufio.Writer) (int, error) {
	var rows *sqlx.Rows
	recordCount := 0

	// first write our normal records
	var record, visibility string

	rows, err := db.QueryxContext(ctx, sqlLookupMsgs, archive.Org.ID, archive.StartDate, archive.endDate())
	if err != nil {
		return 0, fmt.Errorf("error querying messages for org: %d: %w", archive.Org.ID, err)
	}
	defer rows.Close()

	for rows.Next() {
		err = rows.Scan(&visibility, &record)
		if err != nil {
			return 0, fmt.Errorf("error scanning message row for org: %d: %w", archive.Org.ID, err)
		}

		if visibility == "deleted" {
			continue
		}
		writer.WriteString(record)
		writer.WriteString("\n")
		recordCount++
	}

	slog.Debug("Done Writing", "record_count", recordCount)
	return recordCount, nil
}

const sqlSelectOrgMessagesInRange = `
   SELECT mm.id, mm.folder
     FROM msgs_msg mm
LEFT JOIN contacts_contact cc ON cc.id = mm.contact_id
    WHERE mm.org_id = $1 AND mm.created_on >= $2 AND mm.created_on < $3
 ORDER BY mm.created_on ASC, mm.id ASC`

const sqlDeleteMessageLabels = `
DELETE FROM msgs_msg_labels WHERE msg_id IN(?)`

const sqlDeleteMessages = `
DELETE FROM msgs_msg WHERE id IN(?)`

// DeleteArchivedMessages takes the passed in archive, verifies the S3 file is still present (and correct), then selects
// all the messages in the archive date range, and if equal or fewer than the number archived, deletes them 100 at a time
//
// Upon completion it updates the needs_deletion flag on the archive
func DeleteArchivedMessages(ctx context.Context, rt *runtime.Runtime, archive *Archive) error {
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
	log.Info("deleting messages")

	// make sure our archive file is still correct on S3 before we delete anything
	if err := verifyUploadedArchive(outer, rt, archive); err != nil {
		return err
	}

	// ok, archive file looks good, let's build up our list of message ids, this may be big but we are int64s so shouldn't be too big
	rows, err := rt.DB.QueryxContext(outer, sqlSelectOrgMessagesInRange, archive.OrgID, archive.StartDate, archive.endDate())
	if err != nil {
		return err
	}
	defer rows.Close()

	visibleCount := 0
	msgIDs := make([]int64, 0, archive.RecordCount)

	for rows.Next() {
		var msgID int64
		var folder string
		if err := rows.Scan(&msgID, &folder); err != nil {
			return err
		}

		msgIDs = append(msgIDs, msgID)

		// keep track of the number of visible messages, ie, not deleted
		if folder != folderDeleted {
			visibleCount++
		}
	}
	rows.Close()

	log.Debug("found messages", "msg_count", len(msgIDs))

	// verify we don't see more messages than there are in our archive (fewer is ok)
	if visibleCount > archive.RecordCount {
		return fmt.Errorf("more messages in the database: %d than in archive: %d", visibleCount, archive.RecordCount)
	}

	// ok, delete our messages in batches, we do this in transactions as it spans a few different queries
	for idBatch := range slices.Chunk(msgIDs, deleteTransactionSize) {
		// no single batch should take more than a few minutes
		ctx, cancel := context.WithTimeout(ctx, time.Minute*15)
		defer cancel()

		start := dates.Now()

		// start our transaction
		tx, err := rt.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}

		// first delete any labelings
		if err := executeInQuery(ctx, tx, sqlDeleteMessageLabels, idBatch); err != nil {
			return fmt.Errorf("error removing message labels: %w", err)
		}

		// then delete the messages themselves
		if err := executeInQuery(ctx, tx, sqlDeleteMessages, idBatch); err != nil {
			return fmt.Errorf("error deleting messages: %w", err)
		}

		// commit our transaction
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("error committing message delete transaction: %w", err)
		}

		log.Debug("deleted batch of messages", "elapsed", dates.Since(start), "count", len(idBatch))

		cancel()
	}

	slog.Info("completed deleting messages", "elapsed", dates.Since(start))

	return nil
}

const sqlSelectOldOrgBroadcasts = `
SELECT id
  FROM msgs_broadcast b
 WHERE b.org_id = $1 AND b.created_on < $2 AND b.schedule_id IS NULL AND b.is_active AND NOT EXISTS (SELECT 1 FROM msgs_msg WHERE broadcast_id = b.id)
 LIMIT 1000000;`

// DeleteBroadcasts deletes all broadcasts older than the org's retention period which have no associated messages
func DeleteBroadcasts(ctx context.Context, rt *runtime.Runtime, now time.Time, org Org) error {
	return deleteOrphans(ctx, rt, now, org, orphanDeletion{
		what:      "broadcast",
		selectSQL: sqlSelectOldOrgBroadcasts,
		childSQL: []childDelete{
			{"contacts", `DELETE from msgs_broadcast_contacts WHERE broadcast_id = $1`},
			{"groups", `DELETE from msgs_broadcast_groups WHERE broadcast_id = $1`},
			{"counts", `DELETE from msgs_broadcastmsgcount WHERE broadcast_id = $1`},
		},
		parentSQL: `DELETE from msgs_broadcast WHERE id = $1`,
	})
}
