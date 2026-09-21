# Working in codefly-dev/service-redis

This repo is the codefly **agent** for redis: a Go plugin binary (`main.go` →
`agents.Serve`) implementing core's Agent, Runtime and Builder contracts, so a
workspace can declare a redis service, run it locally, and deploy it to
Kubernetes. It owns two runtimes at parity — a container off a pinned image, and
a Docker-free server provisioned from nix — the readiness handshake, the
kustomize deployment templates, and the pin of the runtime image it ships.

It does **not** own redis itself (upstream `redis:*-alpine`), the resource,
network and configuration model (`codefly-dev/core`), or the `codefly` CLI that
drives it. Ports, endpoints, credentials and naming arrive from core's
configuration flow — this agent consumes them, it does not decide them.

## How to behave

Fleet standard — [handbook#68](https://github.com/obin-ai/handbook/issues/68).
These bite here because this agent sits between core's contracts and a real
server: nearly any failure can be made to go away locally by typing a value core
was supposed to resolve, and the result still boots and serves.

- **A gap in the tooling is a bug in the tooling — never a reason to reach around
  it.** When a step belongs to `codefly`, core, or the upstream image, the answer
  is a capability fixed where it lives, and named in the PR — not a
  hand-assembled substitute, not as a "workaround", not "just this once", not
  "until the capability lands". `Dockerfile` is this rule already applied:
  upstream shipped a vulnerable OpenSSL, so the image is rebuilt with pinned
  patched packages and republished, rather than a trivy suppression.
- **Never hack. Provide the best fix, even when it spans repos.** A defect in the
  resource, network or configuration model is fixed in `codefly-dev/core` and
  consumed here as a `go.mod` bump — not absorbed locally because the PR would
  otherwise be in someone else's repo. When it genuinely cannot be fixed now, the
  deliverable is a precise issue against that owner plus an explicitly labelled
  stopgap — never an unlabelled one.
- **Classify every change that makes something work**, in the PR body: a *fix* at
  the place that owns the behaviour, or a *hack*. A hack does not become a fix by
  working, by being small, by being local, or by the real fix belonging elsewhere.
- **Never hardcode what the system resolves.** The port redis serves on is
  `instance.Port`, resolved from the network mapping; `s.redisPort = 6379` in
  `runtime.go` is only the container-internal port the assigned one maps onto.
  The password comes from the configuration flow (`redis`/`REDIS_PASSWORD`), with
  `service.codefly.yaml` as the local fallback. The native runtime's per-session
  state root is keyed by service location *and* naming scope so concurrent
  invocations cannot overwrite each other's config or data — a fixed path
  substituted for it works until two run at once.
- **Diagnose, do not pattern-match.** "It started working when I set X" is not a
  diagnosis — set X back and confirm it breaks. Do not trust an error message
  before checking its claim. Two live examples here: probing the *container* view
  of the mapping reports `redis is not ready` on a container that is up and
  healthy, because `host.docker.internal` is not a portable hostname on the host
  itself (`runtime.go`, `Init`); and a real-redis run without `-timeout 30m`
  reports `FAIL` as a Go test timeout, not as anything redis did.
- **Say what you did not verify.** Unverified is not the same as working, and this
  suite is built to skip rather than lie. Measured: with the docker daemon down,
  the docker-backed real-server tests skip, only the nix one still runs, and
  `go test ./...` prints `ok` in 30s — the same verdict a full 5-minute run gives.
  A green local suite is not evidence that redis was exercised over both backends;
  say which of the gated suites below you actually ran.

## Build and test

Derived from `.github/workflows/ci.yml` and the shared
`codefly-dev/core/.github/workflows/go-service-ci.yml@main` it calls. Go comes
from `go.mod`; CI disables setup-go's cache because it corrupts the downloaded
toolchain module on restore.

```bash
go build ./...        # CI: build job
go vet ./...
go mod tidy -diff     # fails rather than rewriting go.mod/go.sum
go test ./...         # real redis included; ~30s warm, ~5m30s on a cold image pull
```

Two suites are gated on infrastructure, and the gates differ — read which:

| Command | Gate |
| --- | --- |
| `REDIS_INFRA_TESTS=required go test -run TestRealRedis -count=1 -v -timeout 30m ./...` | runs whenever the backend is there; `required` turns a *missing* one from a skip into a failure (`infrastructure tests are required but docker is not usable`) |
| `REDIS_IMAGE_SBOM_TESTS=required go test -run TestImageSBOM -count=1 -v -timeout 30m ./...` | fully opt-in: unset skips even with docker, keeping `go test ./...` off the network |

Keep `-timeout 30m`. Go's default 10m is not enough on a cold image cache —
measured, `TestRealRedisReadinessOverDocker` alone took 590s on a first pull and
timed out; warm, the whole suite is ~2m.

The `image` job additionally builds the runtime image, smoke-tests it with
`redis-cli ping`, scans it with trivy (`HIGH,CRITICAL`, `--exit-code 1`), and
verifies the pin two independent ways: that the digest is reproducible from these
inputs, and that it is *anonymously* pullable. The `ci` job `needs: image`, so a
broken image stops build, vet and test from reporting at all.

## Where things live

| Path | Owns |
| --- | --- |
| `main.go` | agent identity, `Settings`, configuration → connection string, runtime-image lock parsing |
| `runtime.go` | Runtime contract; backend selection (docker vs nix), network mapping fan-in |
| `nixredis.go` | the Docker-free native server: nix materialization, state roots, file locks |
| `redisprobe.go` | the readiness handshake (RESP `AUTH` + `PING`), shared by both runtimes |
| `redislog.go` | parses redis' own log prefix into `wool` records at a mapped severity |
| `builder.go` | Builder contract: `Create`, `Deploy` (kustomize), `Audit`, `SBOM`, `Upgrade` |
| `Dockerfile`, `runtime-image.json` | the shipped runtime image and the lock that pins it |
| `nix/flake.nix` | redis for the native runtime — the other half of runtime parity |
| `templates/deployment` | kustomize base + environment overlay rendered by `Deploy` |
| `templates/factory` | what a newly created redis service gets |

## Rules that bite

- **Readiness is an authenticated `PING`, never a TCP connect.** An open port, a
  `-NOAUTH`, or a `-LOADING` do not prove the agent can run commands. The probe
  separates *retryable* (still coming up) from *terminal* (bad credentials, not a
  redis) — waiting never turns a non-RESP peer into a redis server.
- **One redis process serves every declared TCP alias.** A read/write topology
  declares several TCP endpoints; the runtime folds them onto one instance and
  reports that instance's real addresses for each alias, and `Deploy` publishes
  one Service port per alias, all targeting 6379. Do not give an alias its own
  server or its own port.
- **`runtime-image.json` is a lock, not documentation.** Publish the image before
  you pin its digest: the reproducibility check passes on a pin that was never
  pushed, and only the anonymous-pull check catches it. See the
  `runtime-image-pin` skill.
- **The platform list in the lock is load-bearing.** CI builds exactly those
  platforms, so the digest comparison is what proves the published index ships
  them, and image SBOM evidence owes one subject per platform.
- **Keep both runtimes at parity.** Moving the image without moving
  `nix/flake.lock` leaves the native runtime on a different redis.
- **Do not mock the server.** The socket fixtures in `redisprobe_test.go` pin the
  protocol; only `TestRealRedis` proves redis agrees with it. If a boundary is
  hard to reach, reach it anyway.

## Procedures

Step-by-step procedures live in `.claude/skills/`, loaded on demand:

- `runtime-image-pin` — moving the pinned runtime image, and the publish-before-
  pin ordering that CI checks two different ways.

## Workflow

- Branch and PR; never commit to `main`. Conventional Commits for the title.
- `agentcontext_test.go` holds this file's invariants — the length budget, that
  cited paths resolve, skill frontmatter, and that any `CLAUDE.md` stays a
  pointer. It runs under `go test ./...`, so CI enforces it with no workflow
  change.
- Keep this file under ~150 lines (hard cap 200). Push depth into
  `.claude/skills/` or a nested `AGENTS.md` beside what it describes.
- Treat this file as code: the PR that changes a process updates it.
