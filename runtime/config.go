package runtime

// Config is our top level configuration object
type Config struct {
	DB        string `help:"the connection string for our database"`
	Valkey    string `help:"the connection string for our Valkey instance, used for locking"`
	LogLevel  string `help:"the log level, one of error, warn, info, debug"`
	SentryDSN string `help:"the sentry configuration to log errors to, if any"`

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

		LogLevel: "info",
	}
}
