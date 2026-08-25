package runtime_test

import (
	"log/slog"
	"testing"

	"github.com/nyaruka/archiver/v26/runtime"
	"github.com/stretchr/testify/assert"
)

func TestLoadConfig(t *testing.T) {
	// caller can customize the base config..
	base := runtime.NewDefaultConfig()
	base.S3Bucket = "my-archives"
	base.LogLevel = slog.LevelError

	cfg, err := runtime.LoadConfig(base, `--log-level=warn`)
	assert.NoError(t, err)
	assert.Equal(t, "my-archives", cfg.S3Bucket)
	assert.Equal(t, slog.LevelWarn, cfg.LogLevel)

	// but explicitly set values still take precedence
	base = runtime.NewDefaultConfig()
	base.S3Bucket = "my-archives"

	cfg, err = runtime.LoadConfig(base, `--s3-bucket=other-archives`)
	assert.NoError(t, err)
	assert.Equal(t, "other-archives", cfg.S3Bucket)

	// invalid values are rejected
	_, err = runtime.LoadConfig(runtime.NewDefaultConfig(), `--log-level=bogus`)
	assert.Error(t, err)
}
