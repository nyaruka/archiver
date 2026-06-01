FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/archiver ./cmd/archiver

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/archiver /archiver
ENTRYPOINT ["/archiver"]
