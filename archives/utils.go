package archives

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nyaruka/archiver/v26/runtime"
	"github.com/nyaruka/gocommon/dates"
	"github.com/vinovest/sqlx"
)

// helper method to safely execute an IN query in the passed in transaction
func executeInQuery(ctx context.Context, tx *sqlx.Tx, query string, ids []int64) error {
	q, vs, err := sqlx.In(query, ids)
	if err != nil {
		return err
	}
	q = tx.Rebind(q)

	if _, err := tx.ExecContext(ctx, q, vs...); err != nil {
		tx.Rollback()
	}
	return err
}

// orphanDeletion describes a category of orphaned parent rows to delete (e.g. broadcasts with no
// messages, flow starts with no runs) along with the dependent rows that must be removed first.
type orphanDeletion struct {
	what      string   // singular noun used in logs and errors, e.g. "broadcast"
	selectSQL string   // selects the ids of orphaned parents; takes ($1 org id, $2 threshold)
	childSQL  []string // statements deleting dependent rows, run before the parent; each takes the parent id as $1
	parentSQL string   // statement deleting the parent row; takes the parent id as $1
}

// deleteOrphans deletes orphaned parent rows older than the org's retention period, one transaction
// per parent (parent plus its dependent rows), stopping after an hour so a single org can't
// monopolise a run.
func deleteOrphans(ctx context.Context, rt *runtime.Runtime, now time.Time, org Org, d orphanDeletion) error {
	start := dates.Now()
	threshold := now.AddDate(0, 0, -org.RetentionPeriod)

	rows, err := rt.DB.QueryxContext(ctx, d.selectSQL, org.ID, threshold)
	if err != nil {
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		if count == 0 {
			slog.Info("deleting "+d.what+"s", "org_id", org.ID)
		}

		// been deleting this org more than an hour? that's enough for today, exit out
		if dates.Since(start) > time.Hour {
			break
		}

		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("unable to get %s id: %w", d.what, err)
		}

		// delete each parent and its dependent rows in its own transaction
		tx, err := rt.DB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("error starting transaction while deleting %s: %d: %w", d.what, id, err)
		}

		for _, childSQL := range d.childSQL {
			if _, err := tx.Exec(childSQL, id); err != nil {
				tx.Rollback()
				return fmt.Errorf("error deleting dependent rows for %s: %d: %w", d.what, id, err)
			}
		}

		if _, err := tx.Exec(d.parentSQL, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("error deleting %s: %d: %w", d.what, id, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("error deleting %s: %d: %w", d.what, id, err)
		}

		count++
	}

	if count > 0 {
		slog.Info("completed deleting "+d.what+"s", "elapsed", dates.Since(start), "count", count, "org_id", org.ID)
	}

	return nil
}

// counts the records in the given archives
func countRecords(as []*Archive) int {
	n := 0
	for _, a := range as {
		n += a.RecordCount
	}
	return n
}

// removes duplicates from a slice of archives
func removeDuplicates(as []*Archive) []*Archive {
	unique := make([]*Archive, 0, len(as))
	seen := make(map[string]bool)

	for _, a := range as {
		key := fmt.Sprintf("%s:%s:%s", a.ArchiveType, a.Period, a.StartDate.Format(time.RFC3339))
		if !seen[key] {
			unique = append(unique, a)
			seen[key] = true
		}
	}
	return unique
}
