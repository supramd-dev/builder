#!/usr/bin/env bash
# seed-demo.sh — populate a running md-builder server with demo dashboard
# data: simulated git pushes (webhook) and test-run reports for a couple of
# environments, so the test dashboard matrix can be inspected immediately.
#
# Usage:
#   scripts/seed-demo.sh                          # against http://localhost:8080
#   BASE_URL=http://localhost:9000 scripts/seed-demo.sh
#
# Environment variables:
#   BASE_URL       server base URL          (default http://localhost:8080)
#   SEED_USERNAME  user for login/reporting (default smoke-user; USERNAME is
#                  avoided because fish/POSIX shells treat it as read-only)
#   SEED_PASSWORD  user password            (default smoke-pass-123)
#   PROJECT        fake GitLab project path (default group/md-code)
#
# The script is idempotent-ish: pushes for the same SHAs are deduplicated by
# the server; run reports for the same (env, commit, kind) are replaced.

set -u

BASE_URL="${BASE_URL:-http://localhost:8080}"
USERNAME="${SEED_USERNAME:-smoke-user}"
PASSWORD="${SEED_PASSWORD:-smoke-pass-123}"
PROJECT="${PROJECT:-group/md-code}"

CJAR="$(mktemp)"
trap 'rm -f "$CJAR"' EXIT

die() {
  echo "seed-demo: $*" >&2
  exit 1
}

command -v curl >/dev/null || die "curl is required"
command -v jq >/dev/null || die "jq is required"

echo "seed-demo: logging in as $USERNAME"
code=$(curl -s -o /dev/null -w '%{http_code}' -c "$CJAR" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}" \
  "$BASE_URL/api/login")
[ "$code" = "200" ] || die "login failed (HTTP $code) — create the user first, e.g.: make adduser USER=$USERNAME EMAIL=demo@example.com"

# --- environments ------------------------------------------------------------
# Pick the first two environments owned by the user.
envs=$(curl -s -b "$CJAR" "$BASE_URL/api/environments" | jq -r '.environments[].id')
ENV1=$(head -n1 <<<"$envs")
ENV2=$(tail -n1 <<<"$envs")

if [ -z "$ENV1" ]; then
  die "no environments found — create one in the user center first"
fi
echo "seed-demo: using environments #$ENV1$( [ -n "$ENV2" ] && [ "$ENV2" != "$ENV1" ] && echo " and #$ENV2" )"

# --- pushes (webhook) ---------------------------------------------------------
push() {
  local sha="$1" msg="$2" author="$3"
  curl -s -H 'Content-Type: application/json' -H 'X-Gitlab-Event: Push Hook' \
    -d "$(jq -n --arg project "$PROJECT" --arg sha "$sha" --arg msg "$msg" --arg author "$author" '{
      object_kind: "push",
      project: {path_with_namespace: $project},
      ref: "refs/heads/main",
      after: $sha,
      user_name: $author,
      commits: [{id: $sha, message: $msg}]
    }')" \
    "$BASE_URL/api/webhooks/gitlab" | jq -r '.commitId'
}

echo "seed-demo: recording pushes"
C1=$(push "1111111decafbad" "Add velocity Verlet integrator" "alice")
C2=$(push "2222222decafbad" "Fix PBC image remapping" "bob")
C3=$(push "3333333decafbad" "Tune neighbor list skin" "alice")
[ "$C1" -gt 0 ] && [ "$C2" -gt 0 ] && [ "$C3" -gt 0 ] || die "failed to record pushes"

# --- regression runs ----------------------------------------------------------
report() {
  local env="$1" commit="$2" kind="$3" cases="$4"
  curl -s -o /dev/null -w '%{http_code}' -b "$CJAR" \
    -H 'Content-Type: application/json' \
    -d "$(jq -n --argjson env "$env" --argjson commit "$commit" --arg kind "$kind" --argjson cases "$cases" '{
      environmentId: $env, commitId: $commit, kind: $kind, cases: $cases
    }')" \
    "$BASE_URL/api/test-runs"
}

REG_CASES_1='[
  {"name": "lj-argon-nve", "status": "passed", "errorValue": 1.2e-07, "message": "max rel err"},
  {"name": "water-tip4p-npt", "status": "failed", "errorValue": 0.02, "message": "energy drift above threshold"},
  {"name": "argon-liquid-nvt", "status": "passed", "errorValue": 3.4e-06, "message": "max rel err"}
]'
REG_CASES_2='[
  {"name": "lj-argon-nve", "status": "passed", "errorValue": 1.1e-07, "message": "max rel err"},
  {"name": "water-tip4p-npt", "status": "passed", "errorValue": 8.9e-05, "message": "max rel err"},
  {"name": "argon-liquid-nvt", "status": "passed", "errorValue": 3.1e-06, "message": "max rel err"}
]'
REG_CASES_3='[
  {"name": "lj-argon-nve", "status": "passed", "errorValue": 1.3e-07, "message": "max rel err"},
  {"name": "water-tip4p-npt", "status": "passed", "errorValue": 8.5e-05, "message": "max rel err"},
  {"name": "argon-liquid-nvt", "status": "failed", "errorValue": 0.011, "message": "pressure drift"}
]'

echo "seed-demo: reporting regression runs"
for spec in "1:$ENV1:$C1" "2:$ENV1:$C2" "3:$ENV1:$C3"; do
  IFS=: read -r gen env commit <<<"$spec"
  var="REG_CASES_$gen"
  code=$(report "$env" "$commit" regression "${!var}")
  [ "$code" = "201" ] || die "regression report (env $env, commit $commit) failed: HTTP $code"
done

# Second environment: only the newest commit has a run (partial matrix).
code=$(report "$ENV2" "$C3" regression "$REG_CASES_2")
[ "$code" = "201" ] || [ -z "$ENV2" ] || die "regression report (env $ENV2) failed: HTTP $code"

# --- unit runs -----------------------------------------------------------------
UNIT_CASES='[
  {"name": "TestForce::compute", "status": "passed"},
  {"name": "TestForce::virial", "status": "passed"},
  {"name": "TestIntegrate::verlet", "status": "passed"},
  {"name": "TestNeighborList::rebuild", "status": "failed"},
  {"name": "TestPBC::unwrap", "status": "passed"}
]'
UNIT_CASES_ALL='[
  {"name": "TestForce::compute", "status": "passed"},
  {"name": "TestForce::virial", "status": "passed"},
  {"name": "TestIntegrate::verlet", "status": "passed"},
  {"name": "TestNeighborList::rebuild", "status": "passed"},
  {"name": "TestPBC::unwrap", "status": "passed"}
]'

echo "seed-demo: reporting unit runs"
code=$(report "$ENV1" "$C3" unit "$UNIT_CASES")
[ "$code" = "201" ] || die "unit report (env $ENV1) failed: HTTP $code"
if [ -n "$ENV2" ] && [ "$ENV2" != "$ENV1" ]; then
  code=$(report "$ENV2" "$C3" unit "$UNIT_CASES_ALL")
  [ "$code" = "201" ] || die "unit report (env $ENV2) failed: HTTP $code"
fi

echo "seed-demo: done — open $BASE_URL and pick the Dashboard tab"
