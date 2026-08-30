FROM debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

LABEL org.cpgen.execution-protocol="docker-direct-v2" \
      org.cpgen.image-role="runtime" \
      org.cpgen.image-schema="cpgen.image/v1"

RUN groupadd --gid 65532 cpgen \
    && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /nonexistent cpgen

USER 65532:65532
ENTRYPOINT []
CMD []
