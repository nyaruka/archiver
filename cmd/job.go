package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/nyaruka/archiver/v26/archives"
	"github.com/nyaruka/archiver/v26/runtime"
	"github.com/nyaruka/gocommon/aws/cwatch"
	"github.com/nyaruka/vkutil"
	"github.com/vinovest/sqlx"
)

// Job runs a single archival pass over all active orgs and returns when it completes. Configuration is loaded on
// top of the given defaults, e.g. runtime.NewDefaultConfig(). All logging is sent to the given handler, e.g.
// LogHandler(), whose level is set from the loaded config.
func Job(defaults *runtime.Config, version, date string, logHandler slog.Handler) error {
	cfg, err := runtime.LoadConfig(defaults)
	if err != nil {
		return err
	}

	// configure our logger
	logLevel.Set(cfg.LogLevel)
	slog.SetDefault(slog.New(logHandler))

	log := slog.With("comp", "main")
	log.Info("starting archiver", "version", version, "released", date)

	// our settings shouldn't contain a timezone, nothing will work right with this not being a constant UTC
	if strings.Contains(cfg.DB, "TimeZone") {
		return fmt.Errorf("invalid db connection string, do not specify a timezone, archiver always uses UTC")
	}

	// force our DB connection to be in UTC
	if strings.Contains(cfg.DB, "?") {
		cfg.DB += "&TimeZone=UTC"
	} else {
		cfg.DB += "?TimeZone=UTC"
	}

	rt := &runtime.Runtime{
		Config: cfg,
	}

	rt.DB, err = sqlx.Open("postgres", cfg.DB)
	if err != nil {
		return fmt.Errorf("error opening db: %w", err)
	}
	rt.DB.SetMaxOpenConns(2)

	// sqlx.Open doesn't dial — ping to verify connectivity so init fails fast
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := rt.DB.PingContext(pingCtx); err != nil {
		pingCancel()
		return fmt.Errorf("error connecting to db: %w", err)
	}
	pingCancel()
	log.Info("db ok", "state", "starting")

	rt.VK, err = vkutil.NewPool(cfg.Valkey)
	if err != nil {
		return fmt.Errorf("error creating valkey pool: %w", err)
	}

	// NewPool dials lazily — ping to verify connectivity so init fails fast
	vc := rt.VK.Get()
	if _, err := vc.Do("PING"); err != nil {
		vc.Close()
		return fmt.Errorf("error connecting to valkey: %w", err)
	}
	vc.Close()
	log.Info("valkey ok", "state", "starting")

	rt.S3, err = archives.NewS3Client(cfg, true)
	if err != nil {
		return fmt.Errorf("unable to initialize s3 client: %w", err)
	}
	log.Info("s3 bucket ok", "state", "starting")

	// archives are built on disk before upload so check we can write temp files
	if f, err := os.CreateTemp("", "archiver_check"); err != nil {
		return fmt.Errorf("cannot write to temp directory: %w", err)
	} else {
		f.Close()
		os.Remove(f.Name())
	}
	log.Info("tmp file access ok", "state", "starting")

	rt.CW, err = cwatch.NewService(context.Background(), cfg.CloudwatchNamespace, cfg.DeploymentID)
	if err != nil {
		return fmt.Errorf("unable to create cloudwatch service: %w", err)
	}
	log.Info("cloudwatch service ok", "state", "starting")

	// trap SIGTERM/SIGINT so a stop signal (ECS task stop, deploy, spot interruption) cancels
	// in-flight work and lets per-org locks release via defer, instead of hard-killing the process
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := archives.ArchiveActiveOrgs(ctx, rt); err != nil {
		return fmt.Errorf("error archiving: %w", err)
	}

	return nil
}
