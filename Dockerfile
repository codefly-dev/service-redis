# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG REDIS_IMAGE=redis:8.8.0-alpine@sha256:9d317178eceac8454a2284a9e6df2466b93c745529947f0cd42a0fa9609d7005
ARG SOURCE_DATE_EPOCH=0

FROM ${REDIS_IMAGE} AS runtime

RUN apk add --no-cache --upgrade \
    libcrypto3=3.5.8-r0 \
    libssl3=3.5.8-r0 && \
    rm -f /var/log/apk.log

LABEL org.opencontainers.image.source="https://github.com/codefly-dev/service-redis"
