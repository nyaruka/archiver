package runtime

import (
	valkey "github.com/gomodule/redigo/redis"
	"github.com/nyaruka/gocommon/aws/cwatch"
	"github.com/nyaruka/gocommon/aws/s3x"
	"github.com/vinovest/sqlx"
)

type Runtime struct {
	Config *Config
	DB     *sqlx.DB
	VK     *valkey.Pool
	S3     *s3x.Service
	CW     *cwatch.Service
}
