package runtime

import (
	"log/slog"

	"github.com/nyaruka/ezconf"
)

// Config is our top level configuration object
type Config struct {
	DB       string     `help:"the connection string for our database"`
	Valkey   string     `help:"the connection string for our Valkey instance, used for locking"`
	LogLevel slog.Level `help:"the log level, one of error, warn, info, debug"`

	S3Endpoint  string `help:"S3 endpoint we will write archives to"`
	S3Bucket    string `help:"S3 bucket we will write archives to"`
	S3PathStyle bool   `help:"S3 should use path style URLs"`

	CheckS3Hashes bool `help:"whether to check S3 hashes of uploaded archives before deleting records"`

	RetentionPeriod int `help:"the number of days to keep before archiving"`

	CloudwatchNamespace string `help:"the namespace to use for cloudwatch metrics"`
	DeploymentID        string `help:"the deployment identifier to use for metrics"`
}

// NewDefaultConfig returns a new default configuration object
func NewDefaultConfig() *Config {

	return &Config{
		DB:     "postgres://localhost/archiver_test?sslmode=disable",
		Valkey: "valkey://localhost:6379/15",

		S3Endpoint:  "https://s3.amazonaws.com",
		S3Bucket:    "temba-archives",
		S3PathStyle: false,

		CheckS3Hashes: true,

		RetentionPeriod: 90,

		CloudwatchNamespace: "Archiver",
		DeploymentID:        "dev",

		LogLevel: slog.LevelInfo,
	}
}

// LoadConfig loads configuration from a config file, environment variables and command line args, on top of the
// given defaults, e.g. NewDefaultConfig().
func LoadConfig(cfg *Config, args ...string) (*Config, error) {
	loader := ezconf.NewLoader(cfg, "archiver", "Archives RapidPro runs and msgs to S3", []string{"archiver.toml"})
	if len(args) > 0 { // allow tests to pass in args
		loader.SetArgs(args...)
	}
	if err := loader.Load(); err != nil {
		return nil, err
	}

	return cfg, nil
}
