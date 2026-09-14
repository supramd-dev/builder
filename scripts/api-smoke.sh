#!/usr/bin/env bash
# api-smoke.sh — end-to-end API smoke test against a running md-builder
# server. Exercises the auth flow, environment CRUD, enable/disable,
# connectivity test, command exec and script execution.
#
# Usage:
#   scripts/api-smoke.sh                          # against http://localhost:8080
#   BASE_URL=http://localhost:9000 scripts/api-smoke.sh
#
# Environment variables:
#   BASE_URL   server base URL          (default http://localhost:8080)
#   USERNAME   test user                (default smoke-user)
#   PASSWORD   test user password       (default smoke-pass-123)
#   EMAIL      test user email          (default smoke@example.com)
#   SKIP_SETUP set to 1 to skip user creation (user must already exist)
#   MD_BUILDER_BIN prebuilt server binary for adduser (default: go run)
#   DSN        SQLite/Postgres DSN the server uses, for user creation
#              (default: <project root>/server/md-builder.db)
#
# Exit code 0 = all checks passed, 1 = at least one check failed.

set -u

BASE_URL="${BASE_URL:-http://localhost:8080}"
USERNAME="${USERNAME:-smoke-user}"
PASSWORD="${PASSWORD:-smoke-pass-123}"
EMAIL="${EMAIL:-smoke@example.com}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# The server's default DSN is relative to its own working directory
# (server/), so match that when the server runs via `make dev-backend`.
DSN="${DSN:-$ROOT/server/md-builder.db}"

CJAR="$(mktemp)"
trap 'rm -f "$CJAR" /tmp/api-smoke-*.json' EXIT

PASS=0
FAIL=0

# check NAME EXPECTED_ACTUAL EXPECTED_VALUE — compare two strings.
check() {
  local name="$1" actual="$2" expected="$3"
  if [ "$actual" = "$expected" ]; then
    PASS=$((PASS + 1))
    printf 'ok   %s\n' "$name"
  else
    FAIL=$((FAIL + 1))
    printf 'FAIL %s (expected %s, got %s)\n' "$name" "$expected" "$actual"
  fi
}

# req METHOD PATH [BODY] [CODE_VAR] — authenticated request; prints the body.
req() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-s -b "$CJAR" -c "$CJAR" -X "$method"
      -H 'Content-Type: application/json' -w '\n%{http_code}'
      "$BASE_URL$path")
  if [ -n "$body" ]; then
    args+=(-d "$body")
  fi
  curl "${args[@]}"
}

# expect_code NAME HTTP_CODE EXPECTED
expect_code() {
  check "$1" "$2" "$3"
}

# ---------------------------------------------------------------------------
# 0. Health + unauthenticated access
# ---------------------------------------------------------------------------
echo "== health and auth gates =="

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/health")
check "health responds 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/me")
check "me without session 401" "$(tail -n1 <<<"$body_code")" "401"

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/environments")
check "environments without session 401" "$(tail -n1 <<<"$body_code")" "401"

# ---------------------------------------------------------------------------
# 1. User setup + login flow
# ---------------------------------------------------------------------------
echo "== login flow =="

if [ "${SKIP_SETUP:-0}" != "1" ]; then
  if [ -n "${MD_BUILDER_BIN:-}" ]; then
    ADDUSER=("$MD_BUILDER_BIN" adduser)
  else
    ADDUSER=(go run . adduser)
  fi
  # The server module lives in server/; -dsn pins the same database the
  # running server uses (relative DSNs resolve against the CWD).
  if ! (cd "$ROOT/server" && MD_BUILDER_DSN="$DSN" "${ADDUSER[@]}" \
      -username "$USERNAME" -email "$EMAIL" -password "$PASSWORD") >/dev/null 2>&1; then
    # "already exists" is fine — the user may be left over from a previous run.
    echo "note: adduser failed; assuming user $USERNAME already exists" >&2
  fi
fi

body_code=$(curl -s -c "$CJAR" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USERNAME\",\"password\":\"wrong-pass\"}" \
  -w '\n%{http_code}' "$BASE_URL/api/login")
check "login with wrong password 401" "$(tail -n1 <<<"$body_code")" "401"

body_code=$(curl -s -c "$CJAR" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}" \
  -w '\n%{http_code}' "$BASE_URL/api/login")
check "login 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(curl -s -b "$CJAR" -w '\n%{http_code}' "$BASE_URL/api/me")
check "me with session 200" "$(tail -n1 <<<"$body_code")" "200"
check "me returns username" \
  "$(jq -r .username <<<"$(head -n1 <<<"$body_code")")" "$USERNAME"

# ---------------------------------------------------------------------------
# 2. Environment CRUD
# ---------------------------------------------------------------------------
echo "== environment CRUD =="

FAKE_KEY='-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABFwAAAAdzc2gtcn
NhAAAAAwEAAQAAAQEAtc0vL5cXcWJM1XeA5lCo5XpOw8jKvpFBf4P
-----END OPENSSH PRIVATE KEY-----'

# Invalid payloads are rejected.
body_code=$(req POST /api/environments '{"name":"","host":"","username":"","privateKey":""}')
check "create empty fields 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST /api/environments '{"name":"x","host":"h","username":"u","privateKey":"notapem"}')
check "create non-PEM key 400" "$(tail -n1 <<<"$body_code")" "400"

# Valid create.
printf '{"name":"smoke-node","host":"203.0.113.1","username":"runner","privateKey":%s,"description":"smoke test node"}' \
  "$(jq -Rn --arg k "$FAKE_KEY" '$k')" > /tmp/api-smoke-create.json
body_code=$(req POST /api/environments "$(cat /tmp/api-smoke-create.json)")
check "create environment 201" "$(tail -n1 <<<"$body_code")" "201"
body="$(head -n1 <<<"$body_code")"
ENV_ID=$(jq -r .id <<<"$body")
check "created environment enabled by default" "$(jq -r .enabled <<<"$body")" "true"
check "private key not echoed" "$(jq 'has("privateKey")' <<<"$body")" "false"

# List.
body_code=$(req GET /api/environments)
check "list environments 200" "$(tail -n1 <<<"$body_code")" "200"
check "list contains created env" \
  "$(head -n1 <<<"$body_code" | jq -r --arg id "$ENV_ID" '.environments | map(select(.id == ($id | tonumber))) | length')" \
  "1"

# Get one.
body_code=$(req GET "/api/environments/$ENV_ID")
check "get environment 200" "$(tail -n1 <<<"$body_code")" "200"

# Update (empty privateKey keeps the stored key).
body_code=$(req PUT "/api/environments/$ENV_ID" \
  '{"name":"smoke-node-2","host":"203.0.113.2","username":"runner2","privateKey":"","description":"updated"}')
check "update environment 200" "$(tail -n1 <<<"$body_code")" "200"
check "update persisted name" \
  "$(head -n1 <<<"$body_code" | jq -r .name)" "smoke-node-2"

# Env setup script round-trips verbatim (it is not a secret). Compared
# through a file: the shell's $(...) strips trailing newlines, the env
# script may legitimately end with one.
ENV_SCRIPT_BODY='module load gcc/13
export CXX=g++
'
body_code=$(req PUT "/api/environments/$ENV_ID" "$(jq -n --arg id "$ENV_ID" --arg s "$ENV_SCRIPT_BODY" \
  '{name:"smoke-node-2",host:"203.0.113.2",username:"runner2",privateKey:"",description:"updated",envScript:$s}')")
check "update envScript 200" "$(tail -n1 <<<"$body_code")" "200"
body_code=$(req GET "/api/environments/$ENV_ID")
check "envScript get 200" "$(tail -n1 <<<"$body_code")" "200"
head -n1 <<<"$body_code" | jq -j .envScript > /tmp/api-smoke-envscript.json
if cmp -s /tmp/api-smoke-envscript.json <(printf '%s' "$ENV_SCRIPT_BODY"); then
  check "envScript round-trips" ok ok
else
  check "envScript round-trips" \
    "$(cat /tmp/api-smoke-envscript.json)" "$ENV_SCRIPT_BODY"
fi
# Omitting envScript clears it.
body_code=$(req PUT "/api/environments/$ENV_ID" \
  '{"name":"smoke-node-2","host":"203.0.113.2","username":"runner2","privateKey":"","description":"updated"}')
check "envScript cleared when omitted" \
  "$(head -n1 <<<"$body_code" | jq -r .envScript)" ""

# ---------------------------------------------------------------------------
# 3. Connectivity test
# ---------------------------------------------------------------------------
echo "== connectivity test =="

body_code=$(req POST "/api/environments/$ENV_ID/test")
check "connectivity test 200" "$(tail -n1 <<<"$body_code")" "200"
check "connectivity test fails against fake host" \
  "$(head -n1 <<<"$body_code" | jq -r .success)" "false"

# ---------------------------------------------------------------------------
# 4. Command exec
# ---------------------------------------------------------------------------
echo "== command exec =="

body_code=$(req POST "/api/environments/$ENV_ID/exec" '{"command":"   "}')
check "exec empty command 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST "/api/environments/$ENV_ID/exec" '{"command":"uname -a"}')
check "exec returns 200" "$(tail -n1 <<<"$body_code")" "200"
check "exec against fake host fails" \
  "$(head -n1 <<<"$body_code" | jq -r .success)" "false"
check "exec reports stderr" \
  "$(head -n1 <<<"$body_code" | jq -r '.stderr | length > 0')" "true"

# ---------------------------------------------------------------------------
# 5. Script execution
# ---------------------------------------------------------------------------
echo "== script execution =="

body_code=$(req POST "/api/environments/$ENV_ID/script" '{"language":"bash","script":"  "}')
check "script empty 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST "/api/environments/$ENV_ID/script" '{"language":"perl","script":"print 1;"}')
check "script unsupported language 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST "/api/environments/$ENV_ID/script" \
  '{"language":"bash","script":"#!/usr/bin/env ruby\nputs 1\n"}')
check "script unsupported interpreter 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST "/api/environments/$ENV_ID/script" \
  '{"language":"bash","script":"#!/usr/bin/env bash\necho hello\n"}')
check "bash script 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(req POST "/api/environments/$ENV_ID/script" \
  '{"language":"python","script":"#!/usr/bin/env python3\nprint(1)\n"}')
check "python script 200" "$(tail -n1 <<<"$body_code")" "200"

# ---------------------------------------------------------------------------
# 6. Enable/disable + exec gating
# ---------------------------------------------------------------------------
echo "== enable/disable =="

body_code=$(req PUT "/api/environments/$ENV_ID/enabled" '{}')
check "toggle missing field 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req PUT "/api/environments/$ENV_ID/enabled" '{"enabled":false}')
check "disable 200" "$(tail -n1 <<<"$body_code")" "200"
check "disabled state returned" "$(head -n1 <<<"$body_code" | jq -r .enabled)" "false"

body_code=$(req POST "/api/environments/$ENV_ID/exec" '{"command":"uname -a"}')
check "exec on disabled 409" "$(tail -n1 <<<"$body_code")" "409"

body_code=$(req POST "/api/environments/$ENV_ID/script" '{"language":"bash","script":"echo hi\n"}')
check "script on disabled 409" "$(tail -n1 <<<"$body_code")" "409"

body_code=$(req PUT "/api/environments/$ENV_ID/enabled" '{"enabled":true}')
check "re-enable 200" "$(tail -n1 <<<"$body_code")" "200"
check "enabled state returned" "$(head -n1 <<<"$body_code" | jq -r .enabled)" "true"

# Tags: set on update, echoed back, normalized to lowercase.
body_code=$(req PUT "/api/environments/$ENV_ID" \
  '{"name":"smoke-node-2","host":"203.0.113.2","username":"runner2","privateKey":"","tags":["CPU","mpi CUDA","cpu"],"description":"updated"}')
check "update with tags 200" "$(tail -n1 <<<"$body_code")" "200"
check "tags normalized and deduped" \
  "$(head -n1 <<<"$body_code" | jq -c '.tags')" '["cpu","mpi","cuda"]'

# ---------------------------------------------------------------------------
# 7. Site configuration (GitLab-only repos)
# ---------------------------------------------------------------------------
echo "== site configuration =="

body_code=$(req GET /api/site-config)
check "get site config 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(req PUT /api/site-config '{"codeRepo":""}')
check "config empty fields 400" "$(tail -n1 <<<"$body_code")" "400"

# Repository hosts are not validated (self-hosted GitLab lives on arbitrary
# hosts), so any URL — including other platforms — is accepted.
body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://gitlab.example.com/group/code"}')
check "config self-hosted repo 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://gitlab.com/group/code"}')
check "config update 200" "$(tail -n1 <<<"$body_code")" "200"
check "config update persisted repo" \
  "$(head -n1 <<<"$body_code" | jq -r .codeRepo)" "https://gitlab.com/group/code"

# Access token: a write-only secret. Initial state: unset.
body_code=$(req GET /api/site-config)
check "config token initially unset" \
  "$(head -n1 <<<"$body_code" | jq -r .accessTokenSet)" "false"

# Setting it reports the set-flag without echoing the secret.
body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://gitlab.com/group/code","accessToken":"glpat-smoke-secret"}')
check "config set token 200" "$(tail -n1 <<<"$body_code")" "200"
check "config token reported set" \
  "$(head -n1 <<<"$body_code" | jq -r .accessTokenSet)" "true"
if grep -q "glpat-smoke-secret" <<<"$(head -n1 <<<"$body_code")"; then
  check "config secret not echoed" "leaked" "clean"
else
  check "config secret not echoed" "clean" "clean"
fi

# An update without the token keeps it (empty = keep).
body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://gitlab.com/group/code"}')
check "config keep token 200" "$(tail -n1 <<<"$body_code")" "200"
check "config token kept" \
  "$(head -n1 <<<"$body_code" | jq -r .accessTokenSet)" "true"

# The explicit clear flag removes it.
body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://gitlab.com/group/code","clearAccessToken":true}')
check "config clear token 200" "$(tail -n1 <<<"$body_code")" "200"
check "config token cleared" \
  "$(head -n1 <<<"$body_code" | jq -r .accessTokenSet)" "false"

body_code=$(req GET /api/site-config)
check "config re-read persists" \
  "$(head -n1 <<<"$body_code" | jq -r .codeRepo)" "https://gitlab.com/group/code"

# ---------------------------------------------------------------------------
# 8. GitLab webhook (records a dashboard commit column)
# ---------------------------------------------------------------------------
echo "== gitlab webhook =="

# NOTE: the project path matches the site-config codeRepo above
# (group/code) so the push shows up as a dashboard column under the
# repository filter.
body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Gitlab-Event: Push Hook' \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/code"},"ref":"refs/heads/main","after":"abc123","user_name":"smoke","commits":[{"id":"abc123","message":"smoke push"}]}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook push 200" "$(tail -n1 <<<"$body_code")" "200"
check "webhook push status received" \
  "$(head -n1 <<<"$body_code" | jq -r .status)" "received"
check "webhook push ref extracted" \
  "$(head -n1 <<<"$body_code" | jq -r .ref)" "main"
check "webhook push records commitId" \
  "$(head -n1 <<<"$body_code" | jq -r '.commitId > 0')" "true"
COMMIT_ID=$(head -n1 <<<"$body_code" | jq -r .commitId)

# A repeat of the same push is idempotent (same commit, created=false).
body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Gitlab-Event: Push Hook' \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/code"},"ref":"refs/heads/main","after":"abc123","user_name":"smoke","commits":[{"id":"abc123","message":"smoke push"}]}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook repeat created=false" \
  "$(head -n1 <<<"$body_code" | jq -r .created)" "false"
check "webhook repeat same commitId" \
  "$(head -n1 <<<"$body_code" | jq -r .commitId)" "$COMMIT_ID"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d '{"object_kind":"pipeline"}' "$BASE_URL/api/webhooks/gitlab")
check "webhook other event ignored" \
  "$(head -n1 <<<"$body_code" | jq -r .status)" "ignored"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d '{not-json' "$BASE_URL/api/webhooks/gitlab")
check "webhook bad json 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/webhooks/gitlab")
check "webhook GET 405" "$(tail -n1 <<<"$body_code")" "405"

# The earlier push matched the configured code repository, so the server
# attempted to dispatch jobs. With no reachable git host the dispatch error
# surfaces but the commit is still recorded (HTTP stays 200). Re-check the
# recorded push response by re-pushing the same SHA: the fields must still
# include the dispatch error (idempotent).
body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Gitlab-Event: Push Hook' \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/code"},"ref":"refs/heads/main","after":"abc123","user_name":"smoke","commits":[{"id":"abc123","message":"smoke push"}]}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook dispatch error surfaced" \
  "$(head -n1 <<<"$body_code" | jq 'has("dispatchError")')" "true"

# ---------------------------------------------------------------------------
# 8b. Test dashboard: result reporting, matrix, run detail
# ---------------------------------------------------------------------------
echo "== test dashboard =="

# Unauthenticated dashboard access is rejected.
body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/dashboard/regression")
check "dashboard without session 401" "$(tail -n1 <<<"$body_code")" "401"

# Report a regression run for the created environment at the pushed commit.
body_code=$(req POST /api/test-runs "$(jq -n --argjson env "$ENV_ID" --argjson commit "$COMMIT_ID" '{
  environmentId: $env,
  commitId: $commit,
  kind: "regression",
  startedAt: "2026-09-08T03:00:00Z",
  finishedAt: "2026-09-08T03:04:00Z",
  cases: [
    {"name": "lj-argon-nve", "status": "passed", "errorValue": 1.2e-07, "message": "max rel err"},
    {"name": "water-tip4p-npt", "status": "failed", "errorValue": 0.02, "message": "drift above threshold"}
  ]
}')")
check "report run 201" "$(tail -n1 <<<"$body_code")" "201"
body="$(head -n1 <<<"$body_code")"
RUN_ID=$(jq -r .id <<<"$body")
check "report derives status failed" "$(jq -r .status <<<"$body")" "failed"
check "report derives counts" "$(jq -r '"\(.passed)/\(.total)"' <<<"$body")" "1/2"

# Validation: bad kind, bad case status, missing commit. Use the live
# ENV_ID/COMMIT_ID so re-runs (where env #1 is gone) still hit validation.
body_code=$(req POST /api/test-runs "$(jq -n --argjson env "$ENV_ID" --argjson commit "$COMMIT_ID" '{environmentId: $env, commitId: $commit, kind: "perf"}')")
check "report bad kind 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST /api/test-runs "$(jq -n --argjson env "$ENV_ID" --argjson commit "$COMMIT_ID" '{environmentId: $env, commitId: $commit, kind: "regression", cases: [{name: "a", status: "skipped"}]}')")
check "report bad case status 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST /api/test-runs '{"environmentId":1,"kind":"regression"}')
check "report missing commit 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST /api/test-runs '{"environmentId":999,"commitSha":"nope","kind":"regression"}')
check "report unknown env 404" "$(tail -n1 <<<"$body_code")" "404"

# Report a unit run (counts only, via cases).
body_code=$(req POST /api/test-runs "$(jq -n --argjson env "$ENV_ID" --argjson commit "$COMMIT_ID" '{
  environmentId: $env,
  commitId: $commit,
  kind: "unit",
  cases: [
    {"name": "TestForce", "status": "passed"},
    {"name": "TestIntegrate", "status": "passed"},
    {"name": "TestNeighborList", "status": "failed"}
  ]
}')")
check "report unit run 201" "$(tail -n1 <<<"$body_code")" "201"

# The regression matrix has one row (the push/commit) and one column (the
# environment), and the cell points at the reported run.
body_code=$(req GET /api/dashboard/regression)
check "dashboard regression 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "matrix has one row" "$(jq '.rows | length' <<<"$body")" "1"
check "matrix column is the env" "$(jq -r '.environments[0].name' <<<"$body")" "smoke-node-2"
check "matrix row is the commit" "$(jq -r '.rows[0].commit.shortSha' <<<"$body")" "abc123"
check "matrix cell runId" "$(jq -r '.rows[0].cells[0].runId' <<<"$body")" "$RUN_ID"
check "matrix cell counts" "$(jq -r '.rows[0].cells[0] | "\(.passed)/\(.total)"' <<<"$body")" "1/2"

body_code=$(req GET /api/dashboard/unit)
check "dashboard unit 200" "$(tail -n1 <<<"$body_code")" "200"
check "unit matrix cell counts" \
  "$(head -n1 <<<"$body_code" | jq -r '.rows[0].cells[0] | "\(.passed)/\(.total)"')" "2/3"

# Unknown kind and bad parameter are rejected.
body_code=$(req GET /api/dashboard/other)
check "dashboard unknown kind 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET "/api/dashboard/regression?commits=0")
check "dashboard commits=0 400" "$(tail -n1 <<<"$body_code")" "400"

# The build kind: report a build result and read it back on its own matrix.
body_code=$(req POST /api/test-runs "$(jq -n --argjson env "$ENV_ID" --argjson commit "$COMMIT_ID" '{
  environmentId: $env,
  commitId: $commit,
  kind: "build",
  status: "failed",
  summary: "CMake Error: unknown compiler flag"
}')")
check "report build run 201" "$(tail -n1 <<<"$body_code")" "201"

body_code=$(req GET /api/dashboard/build)
check "dashboard build 200" "$(tail -n1 <<<"$body_code")" "200"
check "build matrix cell failed" \
  "$(head -n1 <<<"$body_code" | jq -r '.rows[0].cells[0].status')" "failed"

# Full matrix: one row per commit with build/unit/regression stages in
# display order; the failed build run above must surface as the build stage.
body_code=$(req GET /api/dashboard/full)
check "dashboard full 200" "$(tail -n1 <<<"$body_code")" "200"
check "full matrix has rows" \
  "$(head -n1 <<<"$body_code" | jq '.rows | length > 0')" "true"
check "full row stages present" \
  "$(head -n1 <<<"$body_code" | jq '.rows[0].stages | length > 0')" "true"
check "full build stage failed" \
  "$(head -n1 <<<"$body_code" | jq -r '[.rows[0].stages[][] | select(.kind == "build").status] | first // empty')" "failed"
check "full build stage has run" \
  "$(head -n1 <<<"$body_code" | jq '[.rows[0].stages[][] | select(.kind == "build").runId] | first > 0')" "true"
check "full environments listed" \
  "$(head -n1 <<<"$body_code" | jq '.environments | length > 0')" "true"

# Run detail carries the case list and commit context.
body_code=$(req GET "/api/test-runs/$RUN_ID")
check "run detail 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "detail environment name" "$(jq -r .environmentName <<<"$body")" "smoke-node-2"
check "detail commit shortSha" "$(jq -r .commitShortSha <<<"$body")" "abc123"
check "detail case count" "$(jq '.cases | length' <<<"$body")" "2"
check "detail case error value" "$(jq -r '.cases[0].errorValue == 1.2e-07' <<<"$body")" "true"

body_code=$(req GET /api/test-runs/9999)
check "run detail unknown 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET /api/test-runs)
check "GET test-runs collection 405" "$(tail -n1 <<<"$body_code")" "405"

# ---------------------------------------------------------------------------
# 8c. Jobs: manual trigger and monitoring
# ---------------------------------------------------------------------------
echo "== jobs =="

# Unauthenticated access is rejected.
body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/jobs")
check "jobs without session 401" "$(tail -n1 <<<"$body_code")" "401"

# Manual trigger for the pushed commit: the dispatch runs again. With no
# reachable git host the trigger reports the failure (422), with the
# counters still present.
body_code=$(req POST /api/jobs "$(jq -n --argjson commit "$COMMIT_ID" '{commitId: $commit}')")
check "trigger dispatch failure 422" "$(tail -n1 <<<"$body_code")" "422"
check "trigger error body present" \
  "$(head -n1 <<<"$body_code" | jq 'has("error")')" "true"

# Validation: missing commit, unknown commit, bad limit.
body_code=$(req POST /api/jobs '{}')
check "trigger missing commit 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST /api/jobs '{"commitId":999999}')
check "trigger unknown commit 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET "/api/jobs?limit=0")
check "jobs bad limit 400" "$(tail -n1 <<<"$body_code")" "400"

# The jobs list endpoint works (empty or populated depending on dispatch
# outcomes; assert shape only).
body_code=$(req GET /api/jobs)
check "jobs list 200" "$(tail -n1 <<<"$body_code")" "200"
check "jobs list is an array" \
  "$(head -n1 <<<"$body_code" | jq '.jobs | type == "array"')" "true"

# ---------------------------------------------------------------------------
# 8d. Task graph API: detail and incremental logs
# ---------------------------------------------------------------------------
echo "== task api =="

# Unauthenticated access is rejected.
body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/tasks/1")
check "task detail without session 401" "$(tail -n1 <<<"$body_code")" "401"

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/tasks/1/log")
check "task logs without session 401" "$(tail -n1 <<<"$body_code")" "401"

# Validation: bad id, unknown task, bad cursor.
body_code=$(req GET /api/tasks/not-a-number)
check "task detail bad id 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req GET /api/tasks/999999)
check "task detail unknown 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET "/api/tasks/999999/log?after=-1")
check "task logs negative after 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req GET "/api/tasks/999999/log")
check "task logs unknown task 404" "$(tail -n1 <<<"$body_code")" "404"

# ---------------------------------------------------------------------------
# 9. Delete + logout (deleting the environment removes its runs too)
# ---------------------------------------------------------------------------
echo "== delete and logout =="

body_code=$(req DELETE "/api/environments/$ENV_ID")
check "delete environment 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(req GET "/api/test-runs/$RUN_ID")
check "run gone after env delete 404" "$(tail -n1 <<<"$body_code")" "404"

# The commit rows may remain, but the environment column is gone and no
# cell holds a run anymore.
body_code=$(req GET /api/dashboard/regression)
check "matrix env column gone after delete" \
  "$(head -n1 <<<"$body_code" | jq '.environments | length')" "0"
check "matrix runs gone after env delete" \
  "$(head -n1 <<<"$body_code" | jq '[.rows[].cells[] | select(. != null)] | length')" "0"

body_code=$(req GET "/api/environments/$ENV_ID")
check "get after delete 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req POST /api/logout)
check "logout 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(curl -s -b "$CJAR" -w '\n%{http_code}' "$BASE_URL/api/me")
check "me after logout 401" "$(tail -n1 <<<"$body_code")" "401"

# ---------------------------------------------------------------------------
echo
echo "passed: $PASS, failed: $FAIL"
[ "$FAIL" -eq 0 ]
