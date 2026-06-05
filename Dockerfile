# syntax=docker/dockerfile:1
# Dev image: builds archiver and runs it on a schedule via supercronic.
#
# Archiver makes a single pass and exits — in production it's driven by an
# external scheduler (e.g. an ECS Scheduled Task). This image is for local
# development: supercronic runs it on the schedule defined below.
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

# version/date are stamped into the binary via ldflags. Pass the git tag at build time
# with --build-arg VERSION=$(git describe --tags) --build-arg RELEASED=$(date -u +%Y-%m-%d);
# the defaults match the in-code fallbacks so a plain `docker build` still works.
ARG VERSION=dev
ARG RELEASED=unknown
RUN go build -ldflags "-X main.version=${VERSION} -X main.date=${RELEASED}" -o /usr/local/bin/archiver ./cmd/archiver

# supercronic is a container-friendly cron: it runs in the foreground, logs to
# stdout, and (unlike system cron) passes the container's env through to jobs.
# go install is the simplest way to get it into a Go image; pin a version so
# the build stays reproducible and this layer caches.
RUN go install github.com/aptible/supercronic@v0.2.46

# supercronic crontab (5-field cron, no user column); times use the container TZ.
# Override at runtime with: docker run -v $PWD/crontab:/etc/crontab ...
COPY <<EOF /etc/crontab
# run a daily archival pass at 06:00 UTC
0 6 * * * /usr/local/bin/archiver
EOF

ENV TZ=Etc/UTC
CMD ["supercronic", "/etc/crontab"]
