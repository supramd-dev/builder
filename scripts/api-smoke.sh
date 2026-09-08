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
#              (default: <project root>/md-builder.db)
#
# Exit code 0 = all checks passed, 1 = at least one check failed.

set -u

BASE_URL="${BASE_URL:-http://localhost:8080}"
USERNAME="${USERNAME:-smoke-user}"
PASSWORD="${PASSWORD:-smoke-pass-123}"
EMAIL="${EMAIL:-smoke@example.com}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DSN="${DSN:-$ROOT/md-builder.db}"

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

# ---------------------------------------------------------------------------
# 7. Site configuration (GitLab-only repos)
# ---------------------------------------------------------------------------
echo "== site configuration =="

body_code=$(req GET /api/site-config)
check "get site config 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(req PUT /api/site-config '{"codeRepo":"","testInputRepo":"","testRepoRef":""}')
check "config empty fields 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://github.com/g/code","testInputRepo":"https://gitlab.com/g/in","testRepoRef":"main"}')
check "config rejects github repo 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://gitlab.com/g/code","testInputRepo":"https://gitlab.com/g/in","testRepoRef":""}')
check "config empty ref 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req PUT /api/site-config \
  '{"codeRepo":"https://gitlab.com/group/code","testInputRepo":"https://gitlab.com/group/test-inputs","testRepoRef":"main"}')
check "config update 200" "$(tail -n1 <<<"$body_code")" "200"
check "config update persisted ref" \
  "$(head -n1 <<<"$body_code" | jq -r .testRepoRef)" "main"

body_code=$(req GET /api/site-config)
check "config re-read persists" \
  "$(head -n1 <<<"$body_code" | jq -r .codeRepo)" "https://gitlab.com/group/code"

# ---------------------------------------------------------------------------
# 8. GitLab webhook
# ---------------------------------------------------------------------------
echo "== gitlab webhook =="

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Gitlab-Event: Push Hook' \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/md-code"},"ref":"refs/heads/main","after":"abc123","user_name":"smoke"}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook push 200" "$(tail -n1 <<<"$body_code")" "200"
check "webhook push status received" \
  "$(head -n1 <<<"$body_code" | jq -r .status)" "received"
check "webhook push ref extracted" \
  "$(head -n1 <<<"$body_code" | jq -r .ref)" "main"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d '{"object_kind":"pipeline"}' "$BASE_URL/api/webhooks/gitlab")
check "webhook other event ignored" \
  "$(head -n1 <<<"$body_code" | jq -r .status)" "ignored"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -d '{not-json' "$BASE_URL/api/webhooks/gitlab")
check "webhook bad json 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/webhooks/gitlab")
check "webhook GET 405" "$(tail -n1 <<<"$body_code")" "405"

# ---------------------------------------------------------------------------
# 9. Delete + logout
# ---------------------------------------------------------------------------
echo "== delete and logout =="

body_code=$(req DELETE "/api/environments/$ENV_ID")
check "delete environment 200" "$(tail -n1 <<<"$body_code")" "200"

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
