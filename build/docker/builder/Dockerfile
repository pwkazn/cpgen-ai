FROM golang:1.24.13-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac

LABEL org.cpgen.execution-protocol="docker-direct-v2" \
      org.cpgen.image-role="builder" \
      org.cpgen.image-schema="cpgen.image/v1" \
      org.cpgen.testlib-sha256="bb323e3c89285214966076e0d23d5a295c5f6126da7ff198c1276ddb95ecb1a0"

COPY third_party/testlib/testlib.h /opt/cpgen/include/testlib.h
RUN test "$(sha256sum /opt/cpgen/include/testlib.h | cut -d ' ' -f 1)" = "bb323e3c89285214966076e0d23d5a295c5f6126da7ff198c1276ddb95ecb1a0" \
    && chmod 0444 /opt/cpgen/include/testlib.h \
    && groupadd --gid 65532 cpgen \
    && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /nonexistent cpgen

USER 65532:65532
ENTRYPOINT []
CMD []
