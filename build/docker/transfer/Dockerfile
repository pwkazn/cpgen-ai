FROM golang:1.24.13-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/cpgen-transfer ./cmd/cpgen-transfer
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -buildvcs=false -mod=readonly -trimpath -ldflags="-s -w -buildid=" \
    -o /out/cpgen-transfer ./cmd/cpgen-transfer

FROM debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

LABEL org.cpgen.execution-protocol="docker-direct-v2" \
      org.cpgen.image-role="transfer" \
      org.cpgen.image-schema="cpgen.image/v1"

COPY --from=build /out/cpgen-transfer /usr/local/bin/cpgen-transfer
RUN chmod 0555 /usr/local/bin/cpgen-transfer \
    && groupadd --gid 65532 cpgen \
    && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /nonexistent cpgen

USER 65532:65532
ENTRYPOINT []
CMD []
