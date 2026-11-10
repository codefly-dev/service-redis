# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG REDIS_IMAGE=redis:8.8.0-alpine@sha256:9d317178eceac8454a2284a9e6df2466b93c745529947f0cd42a0fa9609d7005
ARG SOURCE_DATE_EPOCH=0

FROM ${REDIS_IMAGE} AS runtime

# --upgrade also upgrades each named package's dependencies, so every package
# this transaction changes is named at its exact version: musl floated with the
# Alpine index before it was named here, and musl-utils and zlib stayed at the
# base's vulnerable revisions (CVE-2026-107891 in musl/musl-utils 1.2.5-r23,
# CVE-2026-85091 in zlib 1.3.2-r0), which consumers' image scans block.
RUN apk add --no-cache --upgrade \
    libcrypto3=3.5.9-r0 \
    libssl3=3.5.9-r0 \
    musl=1.2.5-r24 \
    musl-utils=1.2.5-r24 \
    setpriv=2.41.6-r1 \
    zlib=1.4-r0 && \
    rm -f /var/log/apk.log

LABEL org.opencontainers.image.source="https://github.com/codefly-dev/service-redis"
