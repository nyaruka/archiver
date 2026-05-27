package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	_ "github.com/lib/pq"
	"github.com/nyaruka/ezconf"
	"github.com/nyaruka/gocommon/aws/cwatch"
	"github.com/nyaruka/rp-archiver/archives"
	"github.com/nyaruka/rp-archiver/runtime"
	slogmulti "github.com/samber/slog-multi"
	slogsentry "github.com/samber/slog-sentry/v2"
	"github.com/vinovest/sqlx"
)

var (
	// https://goreleaser.com/cookbooks/using-main.version
	version = "dev"
	date    = "unknown"
)

func main() {
	config := runtime.NewDefaultConfig()
	loader := ezconf.NewLoader(&config, "archiver", "Archives RapidPro runs and msgs to S3", []string{"archiver.toml"})
	loader.MustLoad()

	var level slog.Level
	err := level.UnmarshalText([]byte(config.LogLevel))
	if err != nil {
		log.Fatalf("invalid log level %q", config.LogLevel)
	}

	// configure our logger
	logHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(logHandler))

	logger := slog.With("comp", "main")
	logger.Info("starting archiver", "version", version, "released", date)

	// if we have a DSN entry, try to initialize it
	sentryEnabled := false
	if config.SentryDSN != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:           config.SentryDSN,
			EnableTracing: false,
		})
		if err != nil {
			log.Fatalf("error initiating sentry client, error %s, dsn %s", err, config.SentryDSN)
		}
		sentryEnabled = true

		logger = slog.New(
			slogmulti.Fanout(
				logHandler,
				slogsentry.Option{Level: slog.LevelError}.NewSentryHandler(),
			),
		)
		logger = logger.With("release", version)
		slog.SetDefault(logger)
	}

	// fatal exits the process with status 1 after flushing sentry so the ECS task is marked failed
	fatal := func(msg string, args ...any) {
		logger.Error(msg, args...)
		if sentryEnabled {
			sentry.Flush(2 * time.Second)
		}
		os.Exit(1)
	}

	// our settings shouldn't contain a timezone, nothing will work right with this not being a constant UTC
	if strings.Contains(config.DB, "TimeZone") {
		fatal("invalid db connection string, do not specify a timezone, archiver always uses UTC", "db", config.DB)
	}

	// force our DB connection to be in UTC
	if strings.Contains(config.DB, "?") {
		config.DB += "&TimeZone=UTC"
	} else {
		config.DB += "?TimeZone=UTC"
	}

	rt := &runtime.Runtime{
		Config: config,
	}

	rt.DB, err = sqlx.Open("postgres", config.DB)
	if err != nil {
		fatal("error opening db", "error", err)
	}
	rt.DB.SetMaxOpenConns(2)

	// sqlx.Open doesn't dial — ping to verify connectivity so init fails fast
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := rt.DB.PingContext(pingCtx); err != nil {
		pingCancel()
		fatal("error connecting to db", "error", err)
	}
	pingCancel()
	logger.Info("db ok", "state", "starting")

	rt.S3, err = archives.NewS3Client(config, true)
	if err != nil {
		fatal("unable to initialize s3 client", "error", err)
	}
	logger.Info("s3 bucket ok", "state", "starting")

	if err := archives.EnsureTempArchiveDirectory(config.TempDir); err != nil {
		fatal("cannot write to temp directory", "error", err)
	}
	logger.Info("tmp file access ok", "state", "starting")

	rt.CW, err = cwatch.NewService("", "", config.AWSRegion, config.CloudwatchNamespace, config.DeploymentID)
	if err != nil {
		fatal("unable to create cloudwatch service", "error", err)
	}
	logger.Info("cloudwatch service ok", "state", "starting")

	if err := archives.ArchiveActiveOrgs(rt); err != nil {
		fatal("error archiving", "error", err)
	}

	if sentryEnabled {
		sentry.Flush(2 * time.Second)
	}
}
