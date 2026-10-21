#!/usr/bin/env bash
# cache-library-main — TestRealRedisCache* with go/cache (and its cachetest) at
# an UNRELEASED commit of codefly-dev/interface-cache, bumped in this checkout
# only.
#
# ci.yml proves the driver this module requires against the released go/cache
# it requires. interface-cache's own gate (interface-cache#22) compiles the
# newest published driver against every commit of go/cache. A behavioural
# change — a notifier contract tightened, a lease semantic adjusted, a
# retention rule altered — passes both; only the conformance suite on a real
# server sees it, and that suite is this repository's (cacheprovider_test.go),
# on this agent's Runtime. So this is where it runs against main.
#
# The bump is the one the next root-bump pull request would make (`go get`),
# made in the working tree and put back afterwards: nothing committed changes
# what the root module resolves normally. A red run is information about
# go/cache at REF, not a fault in this tree — the library has moved, the driver
# has not yet, and someone decides which side changes. The summary says which.
#
# usage: .github/scripts/cache-library-main.sh [REF] [PATTERN]
#   REF      a branch or commit of codefly-dev/interface-cache, or a go/cache
#            version as go get spells it (v0.4.1); default main
#   PATTERN  the TestRealRedisCache* to run; default TestRealRedisCacheOverDocker$
#
# Locally, an untracked go.work (AGENTS.md) swaps in the working tree's driver
# and the summary says so: that is how a driver port is checked against main
# before either side releases.
set -euo pipefail

library=github.com/codefly-dev/interface-cache
module=$library/go/cache
driver=github.com/codefly-dev/service-redis/cache
ref=${1:-main}
pattern=${2:-'TestRealRedisCacheOverDocker$'}

cd "$(git rev-parse --show-toplevel)"
if ! git diff --quiet -- go.mod go.sum; then
  echo "go.mod or go.sum is already modified: commit or discard that first, or the bump cannot be told from it" >&2
  exit 2
fi
# Whatever happens below, the tree ends as it started.
trap 'git checkout --quiet -- go.mod go.sum' EXIT

# A branch is resolved to its commit here rather than left to the module proxy,
# which may answer a branch query from its cache; a commit or a version goes to
# go get as it is.
target=$ref
if heads=$(git ls-remote --exit-code "https://$library.git" "refs/heads/$ref" 2>/dev/null); then
  target=${heads%%[[:space:]]*}
fi
link="https://$library/commit/$target"
[[ $target =~ ^[0-9a-f]{7,40}$ ]] || link="https://$library"

required=$(go list -m -f '{{.Version}}' "$module")
echo "cache-library-main: go/cache $required is required; bumping to $ref ($target) in the working tree"
go get "$module@$target"
resolved=$(go list -m -f '{{.Version}}' "$module")
echo "cache-library-main: go/cache resolved to $resolved"
git --no-pager diff -- go.mod

driver_version=$(go list -m -f '{{.Version}}' "$driver")
if [ -n "$driver_version" ]; then
  driver_desc="\`$driver\` $driver_version, the published one consumers get"
else
  driver_desc="the working tree's \`cache/\`, through go.work — not the published driver"
fi

log=$(mktemp)
verdict=pass
if ! go vet ./... 2>&1 | tee "$log"; then
  verdict=compile
else
  # required: a runner without the backend fails instead of skipping — a skip
  # here would be a green run that exercised no server (AGENTS.md).
  REDIS_INFRA_TESTS=required go test -run "$pattern" -count=1 -v -timeout 30m ./... 2>&1 | tee "$log" || verdict=behaviour
fi

passed=$(grep -c -- '^ *--- PASS' "$log" || true)
skipped=$(grep -c -- '^ *--- SKIP' "$log" || true)

summary() {
  echo "## cache driver against go/cache \`$ref\` — informational"
  echo
  echo "- go/cache under test: \`$resolved\`, [\`$ref\` of codefly-dev/interface-cache]($link)"
  echo "- go/cache this module requires, which the required CI (\`ci.yml\`) proves on every push: \`$required\`"
  echo "- driver: $driver_desc"
  echo "- suite: \`go test -run '$pattern'\` through this agent's Runtime"
  echo
  if [ "$resolved" = "$required" ]; then
    echo "\`$ref\` is the released \`$required\` this module already requires: nothing unreleased was tested, and this run repeats the required job's verdict. A failure here is this tree's or the runner's, not the library's."
    echo
  fi
  case $verdict in
  pass)
    echo "**Result: the driver behaves against go/cache \`$ref\`** — $passed passed, $skipped skipped."
    ;;
  compile)
    echo "**Result: go/cache \`$ref\` and this driver no longer compile together.** interface-cache's own gate (interface-cache#22) builds the newest published driver against every commit of go/cache, so it is red on this commit for the same reason — or the driver it builds is not the one named above."
    ;;
  behaviour)
    echo "**Result: go/cache \`$ref\` has moved and this driver has not yet.** The same suite passes on the released \`$required\` (the required CI proves it on every push to this repository), so this is not a fault in this repository's tree: the library's unreleased contract and the published driver's behaviour have diverged, and someone has to decide which side changes."
    echo
    echo "- If the library's change is intended, the driver follows: a port in \`cache/\`, a \`cache/\` release, then the root bump — the two-step landing in AGENTS.md (\"This agent provides codefly.dev/cache\")."
    echo "- If it is not, the regression is in codefly-dev/interface-cache, and go/cache should not be released with it."
    echo
    echo "Reproduce on a checkout of this repository: \`.github/scripts/cache-library-main.sh $target\`. What failed:"
    echo
    echo '```'
    grep -- '^ *--- FAIL' "$log" || true
    grep -m1 -E '^(fatal error|panic):' "$log" || true
    echo '```'
    ;;
  esac
}
summary | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"

[ "$verdict" = pass ]
