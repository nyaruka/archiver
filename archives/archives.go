package archives

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/lib/pq"
	"github.com/nyaruka/archiver/v26/runtime"
	"github.com/nyaruka/gocommon/aws/cwatch"
	"github.com/nyaruka/gocommon/dates"
	"github.com/nyaruka/gocommon/uuids"
	"github.com/nyaruka/null/v3"
	"github.com/nyaruka/vkutil/locks"
	"github.com/vinovest/sqlx"
)

// ArchiveType is the type for the archives
type ArchiveType string

const (
	// RunType for run archives
	RunType = ArchiveType("run")

	// MessageType for message archives
	MessageType = ArchiveType("message")
)

// ArchivePeriod is the period of data in the archive
type ArchivePeriod string

const (
	// DayPeriod id the period of a day (24 hours) from archive start date
	DayPeriod = ArchivePeriod("D")

	// MonthPeriod is the period of a month from archive start date
	MonthPeriod = ArchivePeriod("M")
)

// Org represents the model for an org
type Org struct {
	ID              int       `db:"id"`
	Name            string    `db:"name"`
	CreatedOn       time.Time `db:"created_on"`
	IsAnon          bool      `db:"is_anon"`
	RetentionPeriod int
}

// Archive represents the model for an archive
type Archive struct {
	UUID        uuids.UUID  `db:"uuid"`
	ID          int         `db:"id"`
	ArchiveType ArchiveType `db:"archive_type"`
	OrgID       int         `db:"org_id"`
	CreatedOn   time.Time   `db:"created_on"`

	StartDate time.Time     `db:"start_date"`
	Period    ArchivePeriod `db:"period"`

	RecordCount int         `db:"record_count"`
	Size        int64       `db:"size"`
	Hash        null.String `db:"hash"`
	Location    null.String `db:"location"`
	BuildTime   int         `db:"build_time"`

	NeedsDeletion bool       `db:"needs_deletion"`
	DeletedOn     *time.Time `db:"deleted_date"`
	Rollup        *int       `db:"rollup_id"`

	Org         Org
	ArchiveFile string
	Dailies     []*Archive
}

// returns location parsed into bucket and key
func (a *Archive) location() (string, string) {
	parts := strings.SplitN(string(a.Location), ":", 2)
	return parts[0], parts[1]
}

// isUploaded returns true if the archive was uploaded to S3
func (a *Archive) isUploaded() bool {
	return a.Location != ""
}

func (a *Archive) endDate() time.Time {
	endDate := a.StartDate
	if a.Period == DayPeriod {
		endDate = endDate.AddDate(0, 0, 1)
	} else {
		endDate = endDate.AddDate(0, 1, 0)
	}
	return endDate
}

const sqlLookupActiveOrgs = `
  SELECT id, name, created_on, is_anon
    FROM orgs_org
   WHERE is_active
ORDER BY id`

// GetActiveOrgs returns the active organizations sorted by id
func GetActiveOrgs(ctx context.Context, rt *runtime.Runtime) ([]Org, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	rows, err := rt.DB.QueryxContext(ctx, sqlLookupActiveOrgs)
	if err != nil {
		return nil, fmt.Errorf("error fetching active orgs: %w", err)
	}
	defer rows.Close()

	orgs := make([]Org, 0, 100)
	for rows.Next() {
		org := Org{RetentionPeriod: rt.Config.RetentionPeriod}

		if err := rows.StructScan(&org); err != nil {
			return nil, fmt.Errorf("error scanning active org: %w", err)
		}
		orgs = append(orgs, org)
	}

	return orgs, nil
}

const sqlLookupOrgArchives = `
  SELECT uuid, id, org_id, start_date::timestamp with time zone AS start_date, period, archive_type, hash, location, size, record_count, needs_deletion, rollup_id
    FROM archives_archive 
   WHERE org_id = $1 AND archive_type = $2 
ORDER BY start_date ASC, period DESC`

// GetCurrentArchives returns all the current archives for the passed in org and record type
func GetCurrentArchives(ctx context.Context, db *sqlx.DB, org Org, archiveType ArchiveType) ([]*Archive, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	archives := make([]*Archive, 0, 1)
	err := db.SelectContext(ctx, &archives, sqlLookupOrgArchives, org.ID, archiveType)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("error selecting current archives for org: %d and type: %s: %w", org.ID, archiveType, err)
	}

	return archives, nil
}

const sqlLookupArchivesToPurge = `
  SELECT uuid, id, org_id, start_date::timestamp with time zone AS start_date, period, archive_type, hash, location, size, record_count, needs_deletion, rollup_id
    FROM archives_archive 
   WHERE org_id = $1 AND archive_type = $2 AND needs_deletion = TRUE
ORDER BY start_date ASC, period DESC`

// GetArchivesToPurge returns all the archives whose records can be purged
func GetArchivesToPurge(ctx context.Context, db *sqlx.DB, org Org, archiveType ArchiveType) ([]*Archive, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	archives := make([]*Archive, 0, 1)
	err := db.SelectContext(ctx, &archives, sqlLookupArchivesToPurge, org.ID, archiveType)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("error selecting archives to purge for org: %d and type: %s: %w", org.ID, archiveType, err)
	}

	return archives, nil
}

const sqlCountOrgArchives = `
SELECT count(id) 
  FROM archives_archive 
 WHERE org_id = $1 AND archive_type = $2`

// GetCurrentArchiveCount returns the archive count for the passed in org and record type
func GetCurrentArchiveCount(ctx context.Context, db *sqlx.DB, org Org, archiveType ArchiveType) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var archiveCount int

	if err := db.GetContext(ctx, &archiveCount, sqlCountOrgArchives, org.ID, archiveType); err != nil {
		return 0, fmt.Errorf("error querying archive count for org: %d and type: %s: %w", org.ID, archiveType, err)
	}

	return archiveCount, nil
}

// between is inclusive on both sides
const sqlLookupOrgDailyArchivesForDateRange = `
  SELECT uuid, id, org_id, start_date::timestamp with time zone AS start_date, period, archive_type, hash, location, size, record_count, needs_deletion, rollup_id
    FROM archives_archive
   WHERE org_id = $1 AND archive_type = $2 AND period = $3 AND start_date BETWEEN $4 AND $5
ORDER BY start_date ASC`

// GetDailyArchivesForDateRange returns all the current archives for the passed in org and record type and date range
func GetDailyArchivesForDateRange(ctx context.Context, db *sqlx.DB, org Org, archiveType ArchiveType, startDate time.Time, endDate time.Time) ([]*Archive, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	existingArchives := make([]*Archive, 0, 1)

	err := db.SelectContext(ctx, &existingArchives, sqlLookupOrgDailyArchivesForDateRange, org.ID, archiveType, DayPeriod, startDate, endDate)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("error selecting daily archives for org: %d and type: %s: %w", org.ID, archiveType, err)
	}

	return existingArchives, nil
}

// GetMissingDailyArchives calculates what archives need to be generated for the passed in org this is calculated per day
func GetMissingDailyArchives(ctx context.Context, db *sqlx.DB, now time.Time, org Org, archiveType ArchiveType) ([]*Archive, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// our first archive would be active days from today
	endDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -org.RetentionPeriod)
	orgUTC := org.CreatedOn.In(time.UTC)
	startDate := time.Date(orgUTC.Year(), orgUTC.Month(), orgUTC.Day(), 0, 0, 0, 0, time.UTC)

	return GetMissingDailyArchivesForDateRange(ctx, db, startDate, endDate, org, archiveType)
}

const sqlLookupMissingDailyArchive = `
WITH month_days(missing_day) AS (
  select GENERATE_SERIES($1::timestamp with time zone, $2::timestamp with time zone, '1 day')::date
), curr_archives AS (
  SELECT start_date FROM archives_archive WHERE org_id = $3 AND period = $4 AND archive_type=$5
UNION DISTINCT
  -- also get the overlapping days for the monthly rolled up archives
  SELECT GENERATE_SERIES(start_date, (start_date + '1 month'::interval) - '1 second'::interval, '1 day')::date AS start_date
  FROM archives_archive 
  WHERE org_id = $3 AND period = 'M' AND archive_type = $5
)
   SELECT missing_day::timestamp with time zone
     FROM month_days 
LEFT JOIN curr_archives ON curr_archives.start_date = month_days.missing_day
    WHERE curr_archives.start_date IS NULL`

// GetMissingDailyArchivesForDateRange returns all the missing daily archives between the two passed in date ranges
func GetMissingDailyArchivesForDateRange(ctx context.Context, db *sqlx.DB, startDate time.Time, endDate time.Time, org Org, archiveType ArchiveType) ([]*Archive, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	missing := make([]*Archive, 0, 1)

	rows, err := db.QueryxContext(ctx, sqlLookupMissingDailyArchive, startDate, endDate, org.ID, DayPeriod, archiveType)
	if err != nil {
		return nil, fmt.Errorf("error getting missing daily archives for org: %d and type: %s: %w", org.ID, archiveType, err)
	}
	defer rows.Close()

	for rows.Next() {
		var missingDay time.Time
		if err := rows.Scan(&missingDay); err != nil {
			return nil, fmt.Errorf("error scanning missing daily archive for org: %d and type: %s: %w", org.ID, archiveType, err)
		}

		missing = append(missing, &Archive{
			UUID:        uuids.NewV7(),
			Org:         org,
			OrgID:       org.ID,
			StartDate:   missingDay,
			ArchiveType: archiveType,
			Period:      DayPeriod,
		})
	}

	return missing, nil
}

// startDate is truncated to the first of the month
// endDate for range is not inclusive so we must deduct 1 second
const sqlLookupMissingMonthlyArchive = `
WITH month_days(missing_month) AS (
  SELECT generate_series(date_trunc('month', $1::timestamp with time zone), $2::timestamp with time zone - '1 second'::interval, '1 month')::date
), curr_archives AS (
  SELECT start_date FROM archives_archive WHERE org_id = $3 and period = $4 and archive_type = $5
)
   SELECT missing_month::timestamp with time zone 
     FROM month_days 
LEFT JOIN curr_archives ON curr_archives.start_date = month_days.missing_month
    WHERE curr_archives.start_date IS NULL
`

// GetMissingMonthlyArchives gets which monthly archives are currently missing for this org
func GetMissingMonthlyArchives(ctx context.Context, db *sqlx.DB, now time.Time, org Org, archiveType ArchiveType) ([]*Archive, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*5)
	defer cancel()

	lastActive := now.AddDate(0, 0, -org.RetentionPeriod)
	endDate := time.Date(lastActive.Year(), lastActive.Month(), 1, 0, 0, 0, 0, time.UTC)

	orgUTC := org.CreatedOn.In(time.UTC)
	startDate := time.Date(orgUTC.Year(), orgUTC.Month(), 1, 0, 0, 0, 0, time.UTC)

	missing := make([]*Archive, 0, 1)

	rows, err := db.QueryxContext(ctx, sqlLookupMissingMonthlyArchive, startDate, endDate, org.ID, MonthPeriod, archiveType)
	if err != nil {
		return nil, fmt.Errorf("error getting missing monthly archive for org: %d and type: %s: %w", org.ID, archiveType, err)
	}
	defer rows.Close()

	for rows.Next() {
		var missingMonth time.Time
		if err := rows.Scan(&missingMonth); err != nil {
			return nil, fmt.Errorf("error scanning missing monthly archive for org: %d and type: %s: %w", org.ID, archiveType, err)
		}

		missing = append(missing, &Archive{
			UUID:        uuids.NewV7(),
			Org:         org,
			OrgID:       org.ID,
			StartDate:   missingMonth,
			ArchiveType: archiveType,
			Period:      MonthPeriod,
		})
	}

	return missing, nil
}

// BuildRollupArchive builds a monthly archive from the files present on S3
func BuildRollupArchive(ctx context.Context, rt *runtime.Runtime, monthlyArchive *Archive, now time.Time, org Org, archiveType ArchiveType) error {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()

	start := dates.Now()

	// figure out the first day in the monthlyArchive we'll archive
	startDate := monthlyArchive.StartDate
	endDate := startDate.AddDate(0, 1, 0).Add(time.Nanosecond * -1)
	if monthlyArchive.StartDate.Before(org.CreatedOn) {
		orgUTC := org.CreatedOn.In(time.UTC)
		startDate = time.Date(orgUTC.Year(), orgUTC.Month(), orgUTC.Day(), 0, 0, 0, 0, time.UTC)
	}

	// grab all the daily archives we need
	missingDailies, err := GetMissingDailyArchivesForDateRange(ctx, rt.DB, startDate, endDate, org, archiveType)
	if err != nil {
		return err
	}

	if len(missingDailies) != 0 {
		return fmt.Errorf("missing %d daily archives", len(missingDailies))
	}

	// great, we have all the dailies we need, download them
	filename := fmt.Sprintf("%s_%d_%s_%d_%02d_", monthlyArchive.ArchiveType, monthlyArchive.Org.ID, monthlyArchive.Period, monthlyArchive.StartDate.Year(), monthlyArchive.StartDate.Month())
	file, err := os.CreateTemp(rt.Config.TempDir, filename)
	if err != nil {
		return fmt.Errorf("error creating temp file: %s: %w", filename, err)
	}

	success := false
	defer func() {
		// remove the temp file on any early return; only a fully successful build keeps it
		if !success {
			if err := os.Remove(file.Name()); err != nil {
				slog.Error("error cleaning up rollup archive file", "error", err, "filename", file.Name())
			}
		}
	}()

	writerHash := md5.New()
	gzWriter := gzip.NewWriter(io.MultiWriter(file, writerHash))
	writer := bufio.NewWriter(gzWriter)
	defer file.Close()

	recordCount := 0

	dailies, err := GetDailyArchivesForDateRange(ctx, rt.DB, org, archiveType, startDate, endDate)
	if err != nil {
		return err
	}

	// calculate total expected size
	estimatedSize := int64(0)
	for _, d := range dailies {
		estimatedSize += d.Size
	}

	// for each daily
	for _, daily := range dailies {
		// if there are no records in this daily, just move on
		if daily.RecordCount == 0 {
			continue
		}

		bucket, key := daily.location()
		reader, err := GetS3File(ctx, rt.S3, bucket, key)
		if err != nil {
			return fmt.Errorf("error reading daily S3 object: %w", err)
		}

		// set up our reader to calculate our hash along the way
		readerHash := md5.New()
		teeReader := io.TeeReader(reader, readerHash)
		gzipReader, err := gzip.NewReader(teeReader)
		if err != nil {
			return fmt.Errorf("error creating gzip reader: %w", err)
		}

		// copy this daily file (uncompressed) to our new monthly file
		if _, err := io.Copy(writer, gzipReader); err != nil {
			return fmt.Errorf("error copying from S3 to disk %s:%s: %w", bucket, key, err)
		}

		reader.Close()
		gzipReader.Close()

		// check our hash that everything was written out
		hash := hex.EncodeToString(readerHash.Sum(nil))
		if hash != string(daily.Hash) {
			return fmt.Errorf("daily hash mismatch. expected: %s, got %s", daily.Hash, hash)
		}

		recordCount += daily.RecordCount
	}

	if err := writer.Flush(); err != nil {
		return err
	}
	if err := gzWriter.Close(); err != nil {
		return err
	}

	if recordCount > 0 {
		// calculate our size and hash
		monthlyArchive.Hash = null.String(hex.EncodeToString(writerHash.Sum(nil)))
		stat, err := file.Stat()
		if err != nil {
			return fmt.Errorf("error statting file: %s: %w", file.Name(), err)
		}

		monthlyArchive.Size = stat.Size()
	}

	monthlyArchive.RecordCount = recordCount
	monthlyArchive.BuildTime = int(dates.Since(start) / time.Millisecond)
	monthlyArchive.Dailies = dailies
	monthlyArchive.NeedsDeletion = false
	monthlyArchive.ArchiveFile = file.Name()

	success = true
	return nil
}

// EnsureTempArchiveDirectory checks that we can write to our archive directory, creating it first if needbe
func EnsureTempArchiveDirectory(path string) error {
	if len(path) == 0 {
		return fmt.Errorf("path argument cannot be empty")
	}

	// check if path is a directory we can write to
	fileInfo, err := os.Stat(path)
	if os.IsNotExist(err) {
		return os.MkdirAll(path, 0700)
	} else if err != nil {
		return fmt.Errorf("error statting temp dir: %s: %w", path, err)
	}

	// is path a directory
	if !fileInfo.IsDir() {
		return fmt.Errorf("path '%s' is not a directory", path)
	}

	testFilePath := filepath.Join(path, ".test_file")
	testFile, err := os.Create(testFilePath)
	if err != nil {
		return fmt.Errorf("directory '%s' is not writable", path)
	}

	defer testFile.Close()

	err = os.Remove(testFilePath)
	return err
}

// CreateArchiveFile is responsible for writing an archive file for the passed in archive from our database
func CreateArchiveFile(ctx context.Context, db *sqlx.DB, archive *Archive, archivePath string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Hour*3)
	defer cancel()

	start := dates.Now()

	log := slog.With("org_id", archive.Org.ID, "archive_type", archive.ArchiveType, "start_date", archive.StartDate, "end_date", archive.endDate(), "period", archive.Period)

	filename := fmt.Sprintf("%s_%d_%s%d%02d%02d_", archive.ArchiveType, archive.Org.ID, archive.Period, archive.StartDate.Year(), archive.StartDate.Month(), archive.StartDate.Day())
	file, err := os.CreateTemp(archivePath, filename)
	if err != nil {
		return fmt.Errorf("error creating temp file: %s: %w", filename, err)
	}

	success := false
	defer func() {
		// remove the temp file on any early return; only a fully successful build keeps it
		if !success {
			if err := os.Remove(file.Name()); err != nil {
				log.Error("error cleaning up archive file", "error", err, "filename", file.Name())
			}
		}
	}()

	hash := md5.New()
	gzWriter := gzip.NewWriter(io.MultiWriter(file, hash))
	writer := bufio.NewWriter(gzWriter)
	defer file.Close()

	log.Debug("creating new archive file", "filename", file.Name())

	recordCount := 0
	switch archive.ArchiveType {
	case MessageType:
		recordCount, err = writeMessageRecords(ctx, db, archive, writer)
	case RunType:
		recordCount, err = writeRunRecords(ctx, db, archive, writer)
	default:
		err = fmt.Errorf("unknown archive type: %s", archive.ArchiveType)
	}

	if err != nil {
		return fmt.Errorf("error writing archive: %w", err)
	}

	if err := writer.Flush(); err != nil {
		return fmt.Errorf("error flushing archive file: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return fmt.Errorf("error closing archive gzip writer: %w", err)
	}

	if recordCount > 0 {
		// calculate our size and hash
		archive.Hash = null.String(hex.EncodeToString(hash.Sum(nil)))
		stat, err := file.Stat()
		if err != nil {
			return fmt.Errorf("error calculating archive hash: %w", err)
		}

		archive.Size = stat.Size()
	}

	archive.ArchiveFile = file.Name()
	archive.RecordCount = recordCount
	archive.BuildTime = int(dates.Since(start) / time.Millisecond)

	log.Debug("completed writing archive file", "record_count", recordCount, "filename", file.Name(), "file_size", archive.Size, "file_hash", archive.Hash, "elapsed", dates.Since(start))

	success = true
	return nil
}

// UploadArchive uploads the passed archive file to S3
func UploadArchive(ctx context.Context, rt *runtime.Runtime, archive *Archive) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*15)
	defer cancel()

	archivePath := ""
	if archive.Period == DayPeriod {
		archivePath = fmt.Sprintf(
			"%d/%s_%s%d%02d%02d_%s.jsonl.gz",
			archive.Org.ID, archive.ArchiveType, archive.Period,
			archive.StartDate.Year(), archive.StartDate.Month(), archive.StartDate.Day(),
			archive.Hash)
	} else {
		archivePath = fmt.Sprintf(
			"%d/%s_%s%d%02d_%s.jsonl.gz",
			archive.Org.ID, archive.ArchiveType, archive.Period,
			archive.StartDate.Year(), archive.StartDate.Month(),
			archive.Hash)
	}

	if err := UploadToS3(ctx, rt.S3, rt.Config.S3Bucket, archivePath, archive); err != nil {
		return fmt.Errorf("error uploading archive to S3: %w", err)
	}

	archive.NeedsDeletion = archive.RecordCount > 0

	slog.Debug("completed uploading archive file", "org_id", archive.Org.ID, "archive_type", archive.ArchiveType, "start_date", archive.StartDate, "period", archive.Period, "location", archive.Location, "file_size", archive.Size, "file_hash", archive.Hash)

	return nil
}

const sqlInsertArchive = `
INSERT INTO archives_archive(uuid, archive_type, org_id, created_on, start_date, period, record_count, size, hash, location, needs_deletion, build_time, rollup_id)
    VALUES(:uuid, :archive_type, :org_id, :created_on, :start_date, :period, :record_count, :size, :hash, :location, :needs_deletion, :build_time, :rollup_id)
  RETURNING id`

// WriteArchiveToDB write an archive to the Database
func WriteArchiveToDB(ctx context.Context, db *sqlx.DB, archive *Archive) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	archive.OrgID = archive.Org.ID
	archive.CreatedOn = dates.Now()

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("error starting transaction: %w", err)
	}

	rows, err := tx.NamedQuery(sqlInsertArchive, archive)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("error inserting archive: %w", err)
	}

	rows.Next()

	if err := rows.Scan(&archive.ID); err != nil {
		tx.Rollback()
		return fmt.Errorf("error reading new archive id: %w", err)
	}
	rows.Close()

	// if we have children to update do so
	if len(archive.Dailies) > 0 {
		// build our list of ids
		childIDs := make([]int, 0, len(archive.Dailies))
		for _, c := range archive.Dailies {
			childIDs = append(childIDs, c.ID)
		}

		result, err := tx.ExecContext(ctx, `UPDATE archives_archive SET rollup_id = $1 WHERE id = ANY($2)`, archive.ID, pq.Array(childIDs))
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("error updating rollup ids: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("error getting number of rollup ids updated: %w", err)
		}
		if int(affected) != len(childIDs) {
			tx.Rollback()
			return fmt.Errorf("mismatch in number of children updated and number of rows updated")
		}
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return fmt.Errorf("error committing new archive transaction: %w", err)
	}
	return nil
}

// DeleteArchiveTempFile removes our own disk archive file
func DeleteArchiveTempFile(archive *Archive) error {
	if archive.ArchiveFile == "" {
		return nil
	}

	err := os.Remove(archive.ArchiveFile)

	if err != nil {
		return fmt.Errorf("error deleting temp archive file: %s: %w", archive.ArchiveFile, err)
	}

	slog.Debug("deleted temporary archive file", "org_id", archive.Org.ID, "archive_type", archive.ArchiveType, "start_date", archive.StartDate, "period", archive.Period, "db_archive_id", archive.ID, "filename", archive.ArchiveFile)

	return nil
}

// archiveStats holds archiving counts for a single archive type, summable across orgs via add.
type archiveStats struct {
	RecordsArchived int
	ArchivesCreated int
	ArchivesFailed  int
	RollupsCreated  int
	RollupsFailed   int
}

// add accumulates another archiveStats into this one.
func (s *archiveStats) add(o archiveStats) {
	s.RecordsArchived += o.RecordsArchived
	s.ArchivesCreated += o.ArchivesCreated
	s.ArchivesFailed += o.ArchivesFailed
	s.RollupsCreated += o.RollupsCreated
	s.RollupsFailed += o.RollupsFailed
}

// archiveResults holds the archives created, failed and purged for a single org and archive type.
type archiveResults struct {
	dailiesCreated   []*Archive
	dailiesFailed    []*Archive
	monthliesCreated []*Archive
	monthliesFailed  []*Archive
	dailiesPurged    []*Archive
}

// stats summarizes the results into counts for metrics reporting.
func (r archiveResults) stats() archiveStats {
	return archiveStats{
		RecordsArchived: countRecords(r.dailiesCreated),
		ArchivesCreated: len(r.dailiesCreated),
		ArchivesFailed:  len(r.dailiesFailed),
		RollupsCreated:  len(r.monthliesCreated),
		RollupsFailed:   len(r.monthliesFailed),
	}
}

// CreateOrgArchives builds all the missing archives for the passed in org
func CreateOrgArchives(ctx context.Context, rt *runtime.Runtime, now time.Time, org Org, archiveType ArchiveType) (archiveResults, error) {
	var res archiveResults

	archiveCount, err := GetCurrentArchiveCount(ctx, rt.DB, org, archiveType)
	if err != nil {
		return res, fmt.Errorf("error getting current archive count: %w", err)
	}

	// no existing archives means this might be a backfill, figure out if there are full months we can build first
	if archiveCount == 0 {
		archives, err := GetMissingMonthlyArchives(ctx, rt.DB, now, org, archiveType)
		if err != nil {
			return res, fmt.Errorf("error getting missing monthly archives: %w", err)
		}

		// we first create monthly archives
		res.monthliesCreated, res.monthliesFailed = createArchives(ctx, rt, org, archives)
	}

	// then add in daily archives taking into account the monthly that have been built
	daily, err := GetMissingDailyArchives(ctx, rt.DB, now, org, archiveType)
	if err != nil {
		return res, fmt.Errorf("error getting missing daily archives: %w", err)
	}

	// we then create missing daily archives
	res.dailiesCreated, res.dailiesFailed = createArchives(ctx, rt, org, daily)

	return res, nil
}

func createArchive(ctx context.Context, rt *runtime.Runtime, archive *Archive) error {
	if err := CreateArchiveFile(ctx, rt.DB, archive, rt.Config.TempDir); err != nil {
		return fmt.Errorf("error writing archive file: %w", err)
	}

	defer func() {
		if err := DeleteArchiveTempFile(archive); err != nil {
			slog.Error("error deleting temporary archive file", "error", err)
		}
	}()

	// only upload to S3 if there are records
	if archive.RecordCount > 0 {
		if err := UploadArchive(ctx, rt, archive); err != nil {
			return fmt.Errorf("error writing archive to s3: %w", err)
		}
	}

	if err := WriteArchiveToDB(ctx, rt.DB, archive); err != nil {
		return fmt.Errorf("error writing record to db: %w", err)
	}

	return nil
}

func createArchives(ctx context.Context, rt *runtime.Runtime, org Org, archives []*Archive) ([]*Archive, []*Archive) {
	log := slog.With("org_id", org.ID, "org_name", org.Name)

	created := make([]*Archive, 0, len(archives))
	failed := make([]*Archive, 0, 5)

	for _, archive := range archives {
		// stop building on shutdown rather than rapid-failing every remaining archive
		if ctx.Err() != nil {
			break
		}

		log.With("start_date", archive.StartDate, "end_date", archive.endDate(), "period", archive.Period, "archive_type", archive.ArchiveType).Debug("starting archive")
		start := dates.Now()

		if err := createArchive(ctx, rt, archive); err != nil {
			// a cancellation mid-build means we're shutting down, not a real failure — don't log or count it
			if ctx.Err() != nil {
				break
			}
			log.Error("error creating archive", "error", err)
			failed = append(failed, archive)
		} else {
			log.Debug("archive complete", "id", archive.ID, "record_count", archive.RecordCount, "elapsed", dates.Since(start))
			created = append(created, archive)
		}
	}

	return created, failed
}

// RollupOrgArchives rolls up monthly archives from our daily archives
func RollupOrgArchives(ctx context.Context, rt *runtime.Runtime, now time.Time, org Org, archiveType ArchiveType) ([]*Archive, []*Archive, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Hour*3)
	defer cancel()

	log := slog.With("org_id", org.ID, "org_name", org.Name, "archive_type", archiveType)

	// get our missing monthly archives
	archives, err := GetMissingMonthlyArchives(ctx, rt.DB, now, org, archiveType)
	if err != nil {
		return nil, nil, err
	}

	created := make([]*Archive, 0, len(archives))
	failed := make([]*Archive, 0, 1)

	// build them from rollups
	for _, archive := range archives {
		// stop building on shutdown rather than rapid-failing every remaining rollup
		if ctx.Err() != nil {
			break
		}

		log := log.With("start_date", archive.StartDate)
		start := dates.Now()

		if err := BuildRollupArchive(ctx, rt, archive, now, org, archiveType); err != nil {
			// a cancellation mid-build means we're shutting down, not a real failure — don't log or count it
			if ctx.Err() != nil {
				break
			}
			log.Error("error building monthly archive", "error", err)
			failed = append(failed, archive)
			continue
		}

		// only upload to S3 if there are records
		if archive.RecordCount > 0 {
			if err := UploadArchive(ctx, rt, archive); err != nil {
				if ctx.Err() != nil {
					break
				}
				log.Error("error writing archive to s3", "error", err)
				failed = append(failed, archive)
				continue
			}
		}

		if err := WriteArchiveToDB(ctx, rt.DB, archive); err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Error("error writing record to db", "error", err)
			failed = append(failed, archive)
			continue
		}

		if err := DeleteArchiveTempFile(archive); err != nil {
			log.Error("error deleting temporary file", "error", err)
			continue
		}

		log.Info("rollup created", "id", archive.ID, "record_count", archive.RecordCount, "elapsed", dates.Since(start))
		created = append(created, archive)
	}

	return created, failed, nil
}

var deleteTransactionSize = 100

// PurgeArchivedRecords deletes all the database records for the given org based on archives already created
func PurgeArchivedRecords(ctx context.Context, rt *runtime.Runtime, now time.Time, org Org, archiveType ArchiveType) ([]*Archive, error) {
	// get all the archives that haven't yet been purged
	archives, err := GetArchivesToPurge(ctx, rt.DB, org, archiveType)
	if err != nil {
		return nil, fmt.Errorf("error finding archives needing purging '%s'", archiveType)
	}

	purged := make([]*Archive, 0, len(archives))
	for _, a := range archives {
		log := slog.With("archive_id", a.ID, "org_id", a.OrgID, "type", a.ArchiveType, "count", a.RecordCount, "start", a.StartDate, "period", a.Period)

		start := dates.Now()

		switch a.ArchiveType {
		case MessageType:
			err = DeleteArchivedMessages(ctx, rt, a)
			if err == nil {
				err = DeleteBroadcasts(ctx, rt, now, org)
			}
		case RunType:
			err = DeleteArchivedRuns(ctx, rt, a)
			if err == nil {
				err = DeleteFlowStarts(ctx, rt, now, org)
			}
		default:
			err = fmt.Errorf("unknown archive type: %s", a.ArchiveType)
		}
		if err != nil {
			log.Error("error deleting archive records from database", "error", err)
			continue
		}

		purgedOn := dates.Now()

		// mark archive as no longer needing purging
		if _, err := rt.DB.ExecContext(ctx, `UPDATE archives_archive SET needs_deletion = FALSE, deleted_on = $2 WHERE id = $1`, a.ID, purgedOn); err != nil {
			return nil, fmt.Errorf("error setting archive as purged: %w", err)
		}
		a.NeedsDeletion = false
		a.DeletedOn = &purgedOn

		purged = append(purged, a)
		log.Debug("purged archive records", "elapsed", dates.Since(start))
	}

	return purged, nil
}

const sqlSelectDeletableArchives = `
  SELECT uuid, id, org_id, start_date::timestamp with time zone AS start_date, period, archive_type, hash, location, size, record_count, needs_deletion, rollup_id
    FROM archives_archive 
   WHERE org_id = $1 AND archive_type = $2 AND period = 'D' AND rollup_id IS NOT NULL AND NOT needs_deletion`

// DeleteRolledUpDailyArchives deletes daily archives that have been rolled up into monthlies and had their records purged
func DeleteRolledUpDailyArchives(ctx context.Context, rt *runtime.Runtime, org Org, archiveType ArchiveType) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*5)
	defer cancel()

	log := slog.With("org_id", org.ID, "org_name", org.Name, "archive_type", archiveType)

	var toDelete []*Archive
	if err := rt.DB.SelectContext(ctx, &toDelete, sqlSelectDeletableArchives, org.ID, archiveType); err != nil {
		return 0, fmt.Errorf("error selecting rolled up daily archives: %w", err)
	}

	if len(toDelete) == 0 {
		return 0, nil
	}

	// collect IDs and S3 keys grouped by bucket
	ids := make([]int, 0, len(toDelete))
	keysByBucket := make(map[string][]string)
	for _, a := range toDelete {
		ids = append(ids, a.ID)
		if a.isUploaded() {
			bucket, key := a.location()
			keysByBucket[bucket] = append(keysByBucket[bucket], key)
		}
	}

	// delete S3 files
	s3DeletedCount := 0
	for bucket, keys := range keysByBucket {
		deleted, err := DeleteS3Files(ctx, rt.S3, bucket, keys)
		s3DeletedCount += deleted
		if err != nil {
			log.Error("error deleting S3 files for rolled up daily archives", "bucket", bucket, "error", err)
			// continue to try deleting database records
		}
	}

	// delete archives from database by their IDs
	result, err := rt.DB.ExecContext(ctx, `DELETE FROM archives_archive WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return 0, fmt.Errorf("error deleting rolled up daily archives: %w", err)
	}

	deletedCount, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("error getting deleted rows count: %w", err)
	}

	if deletedCount > 0 {
		log.Info("deleted rolled up daily archives", "count", deletedCount, "s3_files_deleted", s3DeletedCount)
	}

	return int(deletedCount), nil
}

// archiveTypeForOrg looks for any missing archives of the given type for the passed in org, creating and uploading them as necessary, returning the created archives
func archiveTypeForOrg(ctx context.Context, rt *runtime.Runtime, now time.Time, org Org, archiveType ArchiveType) (archiveResults, error) {
	log := slog.With("org_id", org.ID, "org_name", org.Name)
	start := dates.Now()

	res, err := CreateOrgArchives(ctx, rt, now, org, archiveType)
	if err != nil {
		return res, fmt.Errorf("error creating archives: %w", err)
	}

	if len(res.dailiesCreated) > 0 {
		elapsed := dates.Since(start)
		rate := float32(countRecords(res.dailiesCreated)) / (float32(elapsed) / float32(time.Second))
		log.Info("completed archival for org", "elapsed", elapsed, "records_per_second", rate)
	}

	rollupsCreated, rollupsFailed, err := RollupOrgArchives(ctx, rt, now, org, archiveType)
	if err != nil {
		return res, fmt.Errorf("error rolling up archives: %w", err)
	}

	res.monthliesCreated = append(res.monthliesCreated, rollupsCreated...)
	// don't double report monthlies that fail being built from db and rolled up from dailies
	res.monthliesFailed = removeDuplicates(append(res.monthliesFailed, rollupsFailed...))

	// purge records from the database for dailies that still need it
	res.dailiesPurged, err = PurgeArchivedRecords(ctx, rt, now, org, archiveType)
	if err != nil {
		return res, fmt.Errorf("error purging archived records: %w", err)
	}

	// delete daily archives that have been rolled up into monthlies and had their records purged
	if _, err := DeleteRolledUpDailyArchives(ctx, rt, org, archiveType); err != nil {
		return res, fmt.Errorf("error deleting rolled up daily archives: %w", err)
	}

	return res, nil
}

// orgResult holds the per-org archiving stats for messages and runs
type orgResult struct {
	msgs archiveStats
	runs archiveStats
}

// ArchiveOrg grabs a per-org lock and archives that org's messages and runs. It returns ok=false
// if the lock couldn't be grabbed or is already held by another task, in which case the org is skipped.
// The lock is always released on the way out via defer, even if archiving panics or returns early.
func ArchiveOrg(ctx context.Context, rt *runtime.Runtime, start time.Time, org Org) (orgResult, bool) {
	log := slog.With("org_id", org.ID, "org_name", org.Name)

	// grab a lock for this org so that overlapping archiver tasks don't archive the same org at once
	locker := locks.NewLocker(fmt.Sprintf("archiver:lock:org:%d", org.ID), time.Hour*13)
	lockCtx, lockCancel := context.WithTimeout(context.Background(), time.Minute)
	lock, err := locker.Grab(lockCtx, rt.VK, 0)
	lockCancel()
	if err != nil {
		log.Error("error grabbing lock for org, skipping", "error", err)
		return orgResult{}, false
	}
	if lock == "" {
		log.Info("org already being archived by another task, skipping")
		return orgResult{}, false
	}

	// release our lock now that we're done with this org
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.Background(), time.Minute)
		if err := locker.Release(relCtx, rt.VK, lock); err != nil {
			log.Error("error releasing lock for org", "error", err)
		}
		relCancel()
	}()

	// no single org should take more than 12 hours; derive from the parent ctx so a shutdown
	// signal cancels in-flight work and the deferred lock release above still runs
	ctx, cancel := context.WithTimeout(ctx, time.Hour*12)
	defer cancel()

	var res orgResult

	msgRes, err := archiveTypeForOrg(ctx, rt, start, org, MessageType)
	// a cancelled context here means we're shutting down, not a real failure — don't report it as an error
	if err != nil && ctx.Err() == nil {
		log.Error("error archiving org messages", "error", err, "archive_type", MessageType)
	}
	res.msgs = msgRes.stats()

	// if we've been cancelled, skip the run pass rather than re-entering the DB helpers on a dead context
	if ctx.Err() != nil {
		return res, true
	}

	runRes, err := archiveTypeForOrg(ctx, rt, start, org, RunType)
	if err != nil && ctx.Err() == nil {
		log.Error("error archiving org runs", "error", err, "archive_type", RunType)
	}
	res.runs = runRes.stats()

	return res, true
}

// ArchiveActiveOrgs fetches active orgs and archives messages and runs
func ArchiveActiveOrgs(ctx context.Context, rt *runtime.Runtime) error {
	start := dates.Now()

	// get our active orgs
	fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
	orgs, err := GetActiveOrgs(fetchCtx, rt)
	cancel()

	if err != nil {
		return fmt.Errorf("error getting active orgs: %w", err)
	}

	var msgs, runs archiveStats

	// for each org, do our export
	for _, org := range orgs {
		// stop cleanly on a shutdown signal so the current org's lock releases via defer
		// rather than lingering until its TTL after a hard kill
		if ctx.Err() != nil {
			slog.Info("shutdown requested, stopping further archival", "reason", ctx.Err(), "next_org_id", org.ID)
			break
		}

		res, ok := ArchiveOrg(ctx, rt, start, org)
		if !ok {
			continue
		}

		msgs.add(res.msgs)
		runs.add(res.runs)
	}

	timeTaken := dates.Now().Sub(start)
	slog.Info("archiving of active orgs complete", "time_taken", timeTaken, "num_orgs", len(orgs))

	msgsDim := cwatch.Dimension("ArchiveType", "msgs")
	runsDim := cwatch.Dimension("ArchiveType", "runs")

	metrics := []types.MetricDatum{
		cwatch.Datum("ArchivingElapsed", timeTaken.Seconds(), types.StandardUnitSeconds),
		cwatch.Datum("RecordsArchived", float64(msgs.RecordsArchived), types.StandardUnitCount, msgsDim),
		cwatch.Datum("RecordsArchived", float64(runs.RecordsArchived), types.StandardUnitCount, runsDim),
		cwatch.Datum("ArchivesCreated", float64(msgs.ArchivesCreated), types.StandardUnitCount, msgsDim),
		cwatch.Datum("ArchivesCreated", float64(runs.ArchivesCreated), types.StandardUnitCount, runsDim),
		cwatch.Datum("ArchivesFailed", float64(msgs.ArchivesFailed), types.StandardUnitCount, msgsDim),
		cwatch.Datum("ArchivesFailed", float64(runs.ArchivesFailed), types.StandardUnitCount, runsDim),
		cwatch.Datum("RollupsCreated", float64(msgs.RollupsCreated), types.StandardUnitCount, msgsDim),
		cwatch.Datum("RollupsCreated", float64(runs.RollupsCreated), types.StandardUnitCount, runsDim),
		cwatch.Datum("RollupsFailed", float64(msgs.RollupsFailed), types.StandardUnitCount, msgsDim),
		cwatch.Datum("RollupsFailed", float64(runs.RollupsFailed), types.StandardUnitCount, runsDim),
	}

	// send metrics on a fresh context so they report even when we're shutting down
	metricsCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	if err = rt.CW.Send(metricsCtx, metrics...); err != nil {
		slog.Error("error sending metrics", "error", err)
	}
	cancel()

	return nil
}
