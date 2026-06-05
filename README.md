# 🗄️ Archiver

[![tag](https://img.shields.io/github/tag/nyaruka/archiver.svg)](https://github.com/nyaruka/archiver/releases)
[![Build Status](https://github.com/nyaruka/archiver/workflows/CI/badge.svg)](https://github.com/nyaruka/archiver/actions?query=workflow%3ACI) 

Task for archiving old [RapidPro](https://app.rapidpro.io)/[TextIt](https://textit.com) runs and messages. It interacts directly with the database and writes archive files to S3.

It runs a single archival pass and then exits. The exit code is meaningful: `0` on success,
non-zero if any initialization step or the archival itself failed. It does not schedule itself —
run it on whatever cadence you need from an external scheduler (cron, a Kubernetes CronJob, an
AWS ECS Scheduled Task driven by EventBridge, etc).

## Configuration

 * `ARCHIVER_DB`: URL describing how to connect to the database
 * `ARCHIVER_VALKEY`: URL describing how to connect to Valkey
 * `ARCHIVER_TEMP_DIR`: The directory that temporary archives will be written before upload

### AWS 

 * `ARCHIVER_AWS_REGION`: AWS region (e.g. `eu-west-1`)

AWS credentials are resolved via the standard AWS SDK default credential chain (instance/task IAM
role, `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` environment variables, shared credentials
file, etc). When running on AWS the task/instance role is the recommended option.

For writing of archives, Archiver needs access to a storage bucket on an S3 compatible service. We recommend that 
you choose SSE-S3 encryption as this is the only type that supports validation of upload ETags.

 * `ARCHIVER_S3_BUCKET`: name of your S3 bucket (e.g. `dl-archiver-test"`)

If using a different encryption type or service that produces non-MD5 ETags:

 * `ARCHIVER_CHECK_S3_HASHES`: can be set to `FALSE` to disable checking of upload hashes.

### Logging and error reporting:

 * `ARCHIVER_DEPLOYMENT_ID`: used for metrics reporting
 * `ARCHIVER_SENTRY_DSN`: DSN to use when logging errors to Sentry
 * `ARCHIVER_LOG_LEVEL`: logging level to use
