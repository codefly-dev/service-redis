---
name: runtime-image-pin
description: Move the pinned redis runtime image in runtime-image.json and Dockerfile — bumping redis or a patched apk package, adding a platform, or clearing a trivy finding. Use when a CVE scan is red, an upgrade is available, or CI reports that the built digest does not match the lock or that the pinned image is not anonymously pullable. Covers the publish-before-pin ordering that CI checks two independent ways.
---

# Moving the pinned runtime image

`runtime-image.json` is a lock, not documentation. It is embedded into the binary
and parsed at startup by `parseRuntimeImageLock`, so a malformed lock fails the
process rather than a request. Four fields, all required: `name`, `tag`, `digest`,
`platforms`.

The image is not upstream redis. `Dockerfile` starts from a digest-pinned
`redis:*-alpine` and reinstalls exact patched apk versions on top, then publishes
as `docker.io/codeflydev/redis`. The tag spells out what is pinned
(`redis-8.8.0-openssl-3.5.8-r0-setpriv-2.41.6-r1-alpine3.23`) so a stale pin is
legible without resolving the digest.

## The ordering that matters

**Publish, then pin.** CI checks the pin two independent ways, and only one of
them catches an unpublished digest:

- *reproducible* — rebuilds from `Dockerfile` and compares to the lock. This
  passes fine on a digest that exists nowhere but your machine.
- *anonymously pullable* — `docker manifest inspect` with an empty
  `DOCKER_CONFIG`. This is the only step that fails on a pin that was never
  pushed, and it exists because anonymous consumers (composition CI) pull this
  image without a registry login.

A pin bumped in the repo but never pushed therefore looks green locally and
401/404s for everyone downstream. Push the multi-arch image to the public
registry first, then write its digest into the lock.

## Steps

1. Change what is pinned in `Dockerfile` — the `REDIS_IMAGE` digest, an apk
   version, or both. Keep versions exact; a floating version defeats the lock.
2. Build and publish the multi-arch image to `docker.io/codeflydev/redis` under a
   tag that spells out the new pins. Needs registry credentials; CI has none, so
   this happens out-of-band from the same inputs.
3. Update `runtime-image.json`: `tag`, `digest` (the *index* digest the push
   reports), and `platforms` if that changed.
4. Reproduce the digest locally with exactly what CI runs — the platform list
   comes from the lock, which is what makes this comparison prove the published
   index ships those platforms:

   ```bash
   platforms=$(jq -er '.platforms | join(",")' runtime-image.json)
   docker buildx build --no-cache \
     --build-arg SOURCE_DATE_EPOCH=0 \
     --platform "$platforms" \
     --provenance=false --sbom=false \
     --output type=oci,dest=/tmp/runtime-oci.tar,rewrite-timestamp=true \
     --metadata-file /tmp/runtime-meta.json \
     .
   jq -er '."containerimage.digest"' /tmp/runtime-meta.json   # must equal .digest
   ```

   A multi-platform build needs a `docker-container` builder — create one with
   `docker buildx create --driver docker-container` and pass `--builder`. Check
   `docker buildx ls` for the driver of the one you use. An existing builder that
   is merely *listed* may not be running; the build then fails on `listing
   workers`, not on anything to do with the image. `docker buildx inspect
   --bootstrap <name>` starts it.

5. Confirm anonymous access the way CI does, so a missing push fails here rather
   than downstream:

   ```bash
   ref=$(jq -er '.name + "@" + .digest' runtime-image.json)
   export DOCKER_CONFIG=$(mktemp -d)
   echo '{}' > "$DOCKER_CONFIG/config.json"
   docker manifest inspect "$ref"
   ```

6. Prove the image has SBOM evidence for every platform it now claims. Subject
   count is derived from the lock, so a platform added without republishing shows
   up here:

   ```bash
   REDIS_IMAGE_SBOM_TESTS=required go test -run TestImageSBOM -count=1 -v -timeout 30m ./...
   ```

7. Exercise a real server on the new image, and keep `-timeout 30m` — a cold pull
   blows through Go's default 10 minutes and reports a test timeout, not a redis
   failure:

   ```bash
   REDIS_INFRA_TESTS=required go test -run TestRealRedis -count=1 -v -timeout 30m ./...
   ```

## Parity and adjacent pins

- **The nix runtime is the other half.** Moving the image without moving
  `nix/flake.lock` leaves the Docker-free runtime on a different redis. Both
  runtimes are supposed to serve the same server.
- **`platforms` drives three things** — what CI builds, what the digest
  comparison proves, and how many SBOM subjects `runtimeImageSubjects` enumerates.
  Adding one without republishing breaks the second and third.
- Subjects carry no digest by design: the pinned reference addresses a manifest
  index, and the scanner binds evidence to each platform's child manifest, which
  never equals the index digest.

## A red trivy scan is not a suppression

`Audit` scans this image for HIGH/CRITICAL findings, and CI fails on them
(`--exit-code 1`). The fix is a newer patched package pinned in `Dockerfile` and a
republished image — the existing OpenSSL pin is that, already applied. If upstream
has no patched version yet, the deliverable is an issue naming the blocker plus an
explicitly labelled stopgap, never a quiet ignore rule.

`codefly` can report an available redis upgrade through the Builder's `Upgrade`
verb (`upgrade.Docker`, current major unless `--major`). Use it to find the target
tag; it does not perform any of the steps above.
