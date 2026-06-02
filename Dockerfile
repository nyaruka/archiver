# Dev image: builds archiver and runs it on a schedule via supercronic.
#
# Archiver makes a single pass and exits — in production it's driven by an
# external scheduler (e.g. an ECS Scheduled Task). This image is for local
# development: supercronic runs it on the schedule in ./crontab.
#
#   docker build -t archiver .
#   docker run --rm -e ARCHIVER_DB=... -e ARCHIVER_VALKEY=... -e ARCHIVER_S3_BUCKET=... archiver
#
# To run a single pass instead of scheduling, override the command:
#   docker run --rm ... archiver archiver

FROM golang:1.26
WORKDIR /usr/src/app

# pre-copy go.mod/go.sum so deps are cached unless they change
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /usr/local/bin/archiver ./cmd/archiver

# supercronic is a container-friendly cron: it runs in the foreground, logs to
# stdout, and (unlike system cron) passes the container's env through to jobs.
# go install is the simplest way to get it into a Go image — no version pinning
# needed for a dev tool.
RUN go install github.com/aptible/supercronic@latest

COPY crontab /etc/crontab
ENV TZ=Etc/UTC
CMD ["supercronic", "/etc/crontab"]
