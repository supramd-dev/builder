#!/usr/bin/env bash
# api-smoke.sh — end-to-end API smoke test against a running md-builder
# server. Exercises the auth flow, environment CRUD, enable/disable,
# connectivity test, command exec, script execution, the webhook, and the
# task/run API (reporting, logs, artifacts) against the demo graphs.
#
# Usage:
#   scripts/api-smoke.sh                          # against http://localhost:8080
#   BASE_URL=http://localhost:9000 scripts/api-smoke.sh
#
# Environment variables:
#   BASE_URL   server base URL          (default http://localhost:8080)
#   USERNAME   test user                (default smoke-user; created as an
#              administrator — by the first-run setup when the database is
#              empty, see §0b, otherwise by adduser in §1)
#   PASSWORD   test user password       (default smoke-pass-123)
#   EMAIL      test user email          (default smoke@example.com)
#   SKIP_SETUP set to 1 to skip user creation (user must already exist)
#   WEBHOOK_TOKEN  the site's webhook secret, for runs against an account
#              that is not an administrator (it is read from
#              /api/site-config otherwise; see §8)
#   MD_BUILDER_BIN prebuilt server binary for adduser and seed (default: go
#              run)
#   SEED       set to 0 to skip seeding the demo data. The reporting checks
#              (§8b and below) write an attempt of a *task*, so they need a
#              task graph on the server; the seed provides one when there is
#              none, exactly as `make seed` does. Skip it for a deployment
#              that cannot run the subcommand — a graph must exist then.
#   DSN        SQLite/Postgres DSN the server uses, for user creation.
#              Must match the DATABASE THE RUNNING SERVER IS ON, or every
#              request 401s (the user is created in a different file).
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
trap 'rm -f "$CJAR" /tmp/api-smoke-*' EXIT

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
# 0b. First-run setup (the guide page, on a database with no account)
# ---------------------------------------------------------------------------
echo "== first-run setup =="

# The state endpoint is unauthenticated — the page it feeds is what a browser
# without a session sees — and reports whether the site has any account yet.
body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/setup")
check "setup state 200" "$(tail -n1 <<<"$body_code")" "200"
check "setup state is a boolean" \
  "$(head -n1 <<<"$body_code" | jq '.required | type == "boolean"')" "true"
SETUP_REQUIRED="$(head -n1 <<<"$body_code" | jq -r '.required')"

if [ "$SETUP_REQUIRED" = "true" ]; then
  # Empty database: the guide page creates the first administrator — this
  # run's $USERNAME, so §1 finds the account already there — and stores the
  # code repository in the same request. No access token: §7 asserts the
  # token starts unset.
  body_code=$(curl -s -c "$CJAR" -H 'Content-Type: application/json' \
    -d "$(jq -n --arg repo "https://gitlab.example.com/smoke/code" \
        --arg u "$USERNAME" --arg e "$EMAIL" --arg p "$PASSWORD" \
        '{codeRepo: $repo, username: $u, email: $e, password: $p}')" \
    -w '\n%{http_code}' "$BASE_URL/api/setup")
  check "setup creates the first administrator 201" "$(tail -n1 <<<"$body_code")" "201"
  check "setup account is an administrator" \
    "$(head -n1 <<<"$body_code" | jq -r .role)" "admin"
  check "setup returns the account" \
    "$(head -n1 <<<"$body_code" | jq -r .username)" "$USERNAME"
  if grep -q "$PASSWORD" <<<"$(head -n1 <<<"$body_code")"; then
    check "setup does not echo the password" "leaked" "clean"
  else
    check "setup does not echo the password" "clean" "clean"
  fi
  check "setup stored the repository" \
    "$(req GET /api/site-config | head -n1 | jq -r .codeRepo)" \
    "https://gitlab.example.com/smoke/code"
  # The new administrator is signed in: the response set a working session.
  check "setup session works" \
    "$(curl -s -b "$CJAR" -o /dev/null -w '%{http_code}' "$BASE_URL/api/me")" "200"
  check "setup no longer required" \
    "$(curl -s "$BASE_URL/api/setup" | jq -r .required)" "false"
else
  echo "note: the database already has an account; only the closed door is checked" >&2
fi

# Whether or not this run started from an empty database, a second setup is
# refused: the endpoint stops doing anything the moment an account exists.
body_code=$(curl -s -H 'Content-Type: application/json' \
  -d '{"codeRepo":"https://gitlab.example.com/other/code","username":"late-admin","email":"late@example.com","password":"late-pass-123"}' \
  -w '\n%{http_code}' "$BASE_URL/api/setup")
check "setup on a configured site 409" "$(tail -n1 <<<"$body_code")" "409"

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
  # running server uses (relative DSNs resolve against the CWD), so adduser
  # needs no config file here.
  #
  # -admin: the webhook checks in §8 need the site's webhook token, which
  # the server only shows to an administrator (pass WEBHOOK_TOKEN to run
  # against a regular account instead).
  if ! (cd "$ROOT/server" && "${ADDUSER[@]}" -dsn "$DSN" -admin \
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

check "list carries the owner" \
  "$(head -n1 <<<"$(req GET /api/environments)" | jq -r --arg id "$ENV_ID" \
     '.environments[] | select(.id == ($id | tonumber)) | .owner')" \
  "$USERNAME"
check "own row is editable" \
  "$(head -n1 <<<"$(req GET /api/environments)" | jq -r --arg id "$ENV_ID" \
     '.environments[] | select(.id == ($id | tonumber)) | .canEdit')" \
  "true"

# --- the pool is site-wide, but other accounts' rows are read-only ---
# A second, non-administrator account: it sees every environment (dispatch
# matches yaml entries against all of them), and may change none of the ones
# it does not own.
echo "== environment ownership =="

OTHER_USER="${OTHER_USER:-smoke-other}"
OTHER_PASS="${OTHER_PASS:-smoke-other-pass-123}"
OTHER_JAR="$(mktemp)"
trap 'rm -f "$CJAR" "$OTHER_JAR" /tmp/api-smoke-*' EXIT

if [ "${SKIP_SETUP:-0}" != "1" ]; then
  if [ -n "${MD_BUILDER_BIN:-}" ]; then
    OTHER_ADDUSER=("$MD_BUILDER_BIN" adduser)
  else
    OTHER_ADDUSER=(go run . adduser)
  fi
  # No -admin: this account must NOT be able to manage another's environment.
  if ! (cd "$ROOT/server" && "${OTHER_ADDUSER[@]}" -dsn "$DSN" \
      -username "$OTHER_USER" -email "other@example.com" -password "$OTHER_PASS") >/dev/null 2>&1; then
    echo "note: adduser failed; assuming user $OTHER_USER already exists" >&2
  fi
fi

body_code=$(curl -s -c "$OTHER_JAR" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$OTHER_USER\",\"password\":\"$OTHER_PASS\"}" \
  -w '\n%{http_code}' "$BASE_URL/api/login")
check "second user login 200" "$(tail -n1 <<<"$body_code")" "200"

# req_other METHOD PATH [BODY] — same as req, with the other account's jar.
req_other() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-s -b "$OTHER_JAR" -c "$OTHER_JAR" -X "$method"
      -H 'Content-Type: application/json' -w '\n%{http_code}'
      "$BASE_URL$path")
  if [ -n "$body" ]; then
    args+=(-d "$body")
  fi
  curl "${args[@]}"
}

body_code=$(req_other GET /api/environments)
check "other user sees the whole pool" \
  "$(head -n1 <<<"$body_code" | jq -r --arg id "$ENV_ID" \
     '.environments | map(select(.id == ($id | tonumber))) | length')" \
  "1"
check "foreign row is read-only" \
  "$(head -n1 <<<"$body_code" | jq -r --arg id "$ENV_ID" \
     '.environments[] | select(.id == ($id | tonumber)) | .canEdit')" \
  "false"

body_code=$(req_other GET "/api/environments/$ENV_ID")
check "other user may read it" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(req_other PUT "/api/environments/$ENV_ID" \
  '{"name":"hijacked","host":"203.0.113.99","username":"runner","privateKey":"","enabled":false}')
check "foreign update 403" "$(tail -n1 <<<"$body_code")" "403"
body_code=$(req_other PUT "/api/environments/$ENV_ID/enabled" '{"enabled":false}')
check "foreign toggle 403" "$(tail -n1 <<<"$body_code")" "403"
body_code=$(req_other POST "/api/environments/$ENV_ID/test" '')
check "foreign connectivity test 403" "$(tail -n1 <<<"$body_code")" "403"
body_code=$(req_other DELETE "/api/environments/$ENV_ID")
check "foreign delete 403" "$(tail -n1 <<<"$body_code")" "403"

# None of the refusals changed anything.
check "refused writes changed nothing" \
  "$(head -n1 <<<"$(req GET "/api/environments/$ENV_ID")" | jq -r .name)" \
  "smoke-node"

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

# The endpoint authenticates with X-Gitlab-Token, and the token is only
# shown to an administrator — which is why §1 creates the smoke user as one.
# Pass WEBHOOK_TOKEN (Settings → Webhook) to run against a regular account.
WEBHOOK_TOKEN="${WEBHOOK_TOKEN:-$(req GET /api/site-config | head -n1 | jq -r '.webhookToken // ""')}"
if [ -z "$WEBHOOK_TOKEN" ]; then
  # Everything below builds on the commit this push records, so a skip is
  # not an option: stop with the reason instead of failing ten checks later.
  echo "error: no webhook token. $USERNAME is not an administrator, so the" >&2
  echo "       server does not show it to them; the dashboard checks below" >&2
  echo "       need the commit this section pushes. Run as an administrator," >&2
  echo "       or pass WEBHOOK_TOKEN=<token from Settings → Webhook>." >&2
  exit 1
fi

# An event without the token is rejected before its payload is parsed.
body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Gitlab-Event: Push Hook' \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/code"}}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook without token 401" "$(tail -n1 <<<"$body_code")" "401"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Gitlab-Token: wrong-token' \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/code"}}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook wrong token 401" "$(tail -n1 <<<"$body_code")" "401"

# NOTE: the project path matches the site-config codeRepo above
# (group/code) so the push shows up as a dashboard column under the
# repository filter.
body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Gitlab-Event: Push Hook' \
  -H "X-Gitlab-Token: $WEBHOOK_TOKEN" \
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
  -H "X-Gitlab-Token: $WEBHOOK_TOKEN" \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/code"},"ref":"refs/heads/main","after":"abc123","user_name":"smoke","commits":[{"id":"abc123","message":"smoke push"}]}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook repeat created=false" \
  "$(head -n1 <<<"$body_code" | jq -r .created)" "false"
check "webhook repeat same commitId" \
  "$(head -n1 <<<"$body_code" | jq -r .commitId)" "$COMMIT_ID"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H "X-Gitlab-Token: $WEBHOOK_TOKEN" \
  -d '{"object_kind":"pipeline"}' "$BASE_URL/api/webhooks/gitlab")
check "webhook other event ignored" \
  "$(head -n1 <<<"$body_code" | jq -r .status)" "ignored"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H "X-Gitlab-Token: $WEBHOOK_TOKEN" \
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
  -H "X-Gitlab-Token: $WEBHOOK_TOKEN" \
  -d '{"object_kind":"push","project":{"path_with_namespace":"group/code"},"ref":"refs/heads/main","after":"abc123","user_name":"smoke","commits":[{"id":"abc123","message":"smoke push"}]}' \
  "$BASE_URL/api/webhooks/gitlab")
check "webhook dispatch error surfaced" \
  "$(head -n1 <<<"$body_code" | jq 'has("dispatchError")')" "true"

# The same failure is stored on the commit row and exposed by the matrix:
# the webhook response is long gone by the time anyone looks at the
# dashboard, which has to explain the commit's empty columns by itself (the
# full view's graph column renders it as a warning icon).
COMMIT_ID=$(head -n1 <<<"$body_code" | jq -r .commitId)
DASH=$(req GET /api/dashboard/full | head -n1)
check "dispatch error recorded on the commit" \
  "$(jq -r --argjson id "$COMMIT_ID" \
     '.rows[] | select(.commit.id == $id) | .commit.dispatchError | length > 0' <<<"$DASH")" "true"
check "failed dispatch leaves no task graph" \
  "$(jq -r --argjson id "$COMMIT_ID" \
     '.rows[] | select(.commit.id == $id) | .taskIds | length' <<<"$DASH")" "0"

# Rotating the webhook secret (administrators only; the smoke user is one)
# issues a new value and retires the old one on the spot. This runs last so
# the checks above keep using the token the site started with.
body_code=$(req POST /api/site-config/webhook-token)
check "webhook token rotate 200" "$(tail -n1 <<<"$body_code")" "200"
ROTATED_TOKEN=$(head -n1 <<<"$body_code" | jq -r .webhookToken)
check "webhook token rotated" \
  "$([ -n "$ROTATED_TOKEN" ] && [ "$ROTATED_TOKEN" != "$WEBHOOK_TOKEN" ] && echo changed)" "changed"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H "X-Gitlab-Token: $WEBHOOK_TOKEN" \
  -d '{"object_kind":"pipeline"}' "$BASE_URL/api/webhooks/gitlab")
check "old token retired 401" "$(tail -n1 <<<"$body_code")" "401"

body_code=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' \
  -H "X-Gitlab-Token: $ROTATED_TOKEN" \
  -d '{"object_kind":"pipeline"}' "$BASE_URL/api/webhooks/gitlab")
check "rotated token accepted 200" "$(tail -n1 <<<"$body_code")" "200"

# ---------------------------------------------------------------------------
# 8b. Task graphs: where the reporting checks below get a task from
# ---------------------------------------------------------------------------
echo "== task graphs =="

# Reporting writes an attempt of a *task* — a node of the graph a dispatch
# built — so every check from here on needs a real graph on a real
# environment. This script's own push cannot produce one: its commit carries a
# made-up SHA and no git host is reachable, so that dispatch fails by design
# (§8). The graphs come from `md-builder seed`, which writes the demo matrix
# through the production graph builder and is idempotent — on a site that
# already has them (or any other graph) it changes no task.
#
# SEED=0 skips the subcommand, for a deployment that cannot run it (no config
# file for object storage, no source tree). A graph has to exist then: the run
# stops with the reason instead of failing twenty checks later.
#
# The demo environments belong to the demo account. This script's own account
# is an administrator, which may manage every environment (§1), so it can
# report into them; §2's second account is not, which is what the 403 check in
# §8d is built on.
SEED="${SEED:-1}"
if [ -n "${MD_BUILDER_BIN:-}" ]; then
  SEEDER=("$MD_BUILDER_BIN" seed)
else
  SEEDER=(go run . seed)
fi

# seed_demo — run the seed subcommand against the server's own database. Its
# stdout carries the demo account's password, so it is discarded; stderr
# carries the reason it failed and is worth showing.
seed_demo() {
  if (cd "$ROOT/server" && "${SEEDER[@]}" -dsn "$DSN") >/dev/null 2>/tmp/api-smoke-seed.err; then
    return 0
  fi
  echo "error: seeding the demo data failed:" >&2
  cat /tmp/api-smoke-seed.err >&2
  echo "       SEED=0 skips the seed, but the checks below then need a task" >&2
  echo "       graph from a dispatch of your own." >&2
  exit 1
}

ROOTS=$(req GET '/api/jobs?limit=100' | head -n1)
if [ "$(jq '.jobs | length' <<<"$ROOTS")" = "0" ] && [ "$SEED" != "0" ]; then
  echo "note: no task graph on this server; seeding the demo data" >&2
  seed_demo
  ROOTS=$(req GET '/api/jobs?limit=100' | head -n1)
fi

if [ "$(jq '.jobs | length' <<<"$ROOTS")" = "0" ]; then
  echo "error: no task graph to report against. POST /api/test-runs addresses a" >&2
  echo "       task, not an environment, so these checks need a graph from a" >&2
  echo "       dispatch: seed the demo data, push to a reachable repository, or" >&2
  echo "       dispatch one by hand (see docs/runner-strategy.md)." >&2
  exit 1
fi

# Pick a finished graph and one of its real nodes: the unit stage, which
# reports counts and fetches its XML back, so it has a run and an artifact to
# download. A site with graphs of its own may have another shape, hence the
# scan down the job list rather than "the newest job".
ROOT_ID="" NODE_ID="" NODE_RUN="" NODE_ATTEMPT=""
CLONE_ID="" STAGE_ID="" GRAPH_REPO="" CELL_ENV="" CELL_COMMIT=""
while read -r root; do
  [ -n "$root" ] || continue
  detail=$(req GET "/api/tasks/$root" | head -n1)
  cand=$(jq -r '[.subTasks[] | select((.virtual // false) == false
            and .kind == "unit" and (.runId // 0) > 0)]
            | first // empty | "\(.id) \(.runId) \(.attempts)"' <<<"$detail")
  clone=$(jq -r '[.subTasks[] | select(.kind == "clone")] | first | .id // empty' <<<"$detail")
  [ -n "$cand" ] && [ -n "$clone" ] || continue
  IFS=' ' read -r nid nrun natt <<<"$cand"
  arts=$(req GET "/api/test-runs/$nrun" | head -n1 | jq -r '.artifacts | length' 2>/dev/null)
  case "$arts" in ''|*[!0-9]*) continue ;; esac
  [ "$arts" -gt 0 ] || continue
  ROOT_ID=$root
  NODE_ID=$nid
  NODE_RUN=$nrun
  NODE_ATTEMPT=$natt
  CLONE_ID=$clone
  STAGE_ID=$(jq -r '[.subTasks[] | select(.kind == "regression")] | first | .id // empty' <<<"$detail")
  GRAPH_REPO=$(jq -r '.commit.repo // empty' <<<"$detail")
  CELL_ENV=$(jq -r .environmentId <<<"$detail")
  CELL_COMMIT=$(jq -r .commitId <<<"$detail")
  break
done <<<"$(jq -r '.jobs[] | select(.status == "passed") | .id' <<<"$ROOTS")"

if [ -z "$NODE_ID" ]; then
  echo "error: no finished graph with a unit stage and a run artifact was found." >&2
  echo "       The checks below report a run and download what it produced;" >&2
  echo "       the demo data (SEED=1, the default) has both." >&2
  exit 1
fi

# The matrix is filtered by the site's code repository, and §7 pointed that at
# the smoke repository — which would hide the very rows these checks read.
# Seeding puts the filter back on the demo repository (the store's seed knows a
# leftover smoke filter hides every row). A filter that still does not cover
# the graph's repository would make the cell checks below test nothing, so
# stop instead.
FILTER=$(req GET /api/dashboard/unit | head -n1 | jq -r '.repoFilter // empty')
if [ "$FILTER" != "$GRAPH_REPO" ]; then
  if [ "$SEED" = "0" ]; then
    echo "error: the matrix is filtered to \"$FILTER\" but the graph belongs" >&2
    echo "       to \"$GRAPH_REPO\", so its rows are hidden. Point the site" >&2
    echo "       config's code repository at it, or let the script seed." >&2
    exit 1
  fi
  echo "note: pointing the site's code repository back at $GRAPH_REPO" >&2
  seed_demo
  FILTER=$(req GET /api/dashboard/unit | head -n1 | jq -r '.repoFilter // empty')
  if [ "$FILTER" != "$GRAPH_REPO" ]; then
    echo "error: the matrix is still filtered to \"$FILTER\"" >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# 8c. Task, run, log and artifact API
# ---------------------------------------------------------------------------
echo "== task, run, log and artifact api =="

# Unauthenticated access is rejected on all of them.
body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/tasks/1")
check "task detail without session 401" "$(tail -n1 <<<"$body_code")" "401"

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/tasks/1/log")
check "task logs without session 401" "$(tail -n1 <<<"$body_code")" "401"

body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/test-artifacts/1")
check "artifact without session 401" "$(tail -n1 <<<"$body_code")" "401"

# Validation: bad ids, unknown rows, a negative cursor, an attempt that is not
# a positive integer. The cursor is checked before the task is looked up, so
# both of the last two answer 400 rather than 404.
body_code=$(req GET /api/tasks/not-a-number)
check "task detail bad id 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req GET /api/tasks/999999)
check "task detail unknown 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET "/api/tasks/999999/log")
check "task logs unknown task 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET "/api/tasks/999999/log?after=-1")
check "task logs negative after 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req GET "/api/tasks/$NODE_ID/log?attempt=0")
check "task logs attempt=0 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req GET /api/test-artifacts/not-a-number)
check "artifact bad id 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req GET /api/test-artifacts/999999)
check "artifact unknown 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET /api/test-runs/not-a-number)
check "run detail bad id 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req GET /api/test-runs/999999)
check "run detail unknown 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET /api/test-runs)
check "GET test-runs collection 405" "$(tail -n1 <<<"$body_code")" "405"

# The root of the graph: virtual, carrying the current nodes and the ones a
# later dispatch dropped. It has no attempt of its own — its status is the
# rollup of the nodes below it.
body_code=$(req GET "/api/tasks/$ROOT_ID")
check "root task detail 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "root task is virtual" "$(jq -r .virtual <<<"$body")" "true"
check "root task node key" "$(jq -r .nodeKey <<<"$body")" "root"
check "root task lists its nodes" "$(jq '.subTasks | length > 0' <<<"$body")" "true"
# Retired nodes are the ones a later dispatch dropped; the field is absent
# while every node the graph ever defined is still in it.
check "root task retired nodes are a list" \
  "$(jq '(.retiredTasks // []) | type == "array"' <<<"$body")" "true"
check "root task has no runs of its own" "$(jq 'has("runs")' <<<"$body")" "false"
check "root task carries the environment" "$(jq -r .environment.id <<<"$body")" "$CELL_ENV"
check "root node lists its latest run" \
  "$(jq -r --argjson id "$NODE_ID" '.subTasks[] | select(.id == $id) | .runId' <<<"$body")" \
  "$NODE_RUN"
# The node list is only attached to a root, which is what makes it the one
# place a container's cases are read from (the check in §8c does that).
ROOT_BODY="$body"

# A real node: one run per attempt, newest first, no children.
body_code=$(req GET "/api/tasks/$NODE_ID")
check "unit task detail 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "unit task is a real node" "$(jq 'has("virtual")' <<<"$body")" "false"
check "unit task kind and key" \
  "$(jq -r '"\(.kind)/\(.nodeKey)"' <<<"$body")" "unit/unit"
check "unit task runs match attempts" \
  "$(jq -r '(.runs | length) == .attempts' <<<"$body")" "true"
check "unit latest run is the linked one" \
  "$(jq -r --argjson id "$NODE_RUN" '.runs[] | select(.id == $id) | .attempt' <<<"$body")" \
  "$NODE_ATTEMPT"
check "unit task has no child nodes" "$(jq 'has("subTasks")' <<<"$body")" "false"

# The same attempts, on the endpoint the log viewer's attempt switcher reads.
body_code=$(req GET "/api/tasks/$NODE_ID/runs")
check "task runs 200" "$(tail -n1 <<<"$body_code")" "200"
check "task runs newest first" \
  "$(head -n1 <<<"$body_code" | jq -r '.runs[0].attempt')" "$NODE_ATTEMPT"

# The regression stage is a virtual container with one case task per preset:
# no run of its own, its status is the cases' aggregate. A node list belongs to
# a root, so its cases are read from the root's (ROOT_BODY), filtered by
# parentId — a case is a task of its own, with its own run.
if [ -n "$STAGE_ID" ]; then
  body_code=$(req GET "/api/tasks/$STAGE_ID")
  check "regression container 200" "$(tail -n1 <<<"$body_code")" "200"
  body="$(head -n1 <<<"$body_code")"
  check "regression container is virtual" "$(jq -r .virtual <<<"$body")" "true"
  check "regression container kind" "$(jq -r .kind <<<"$body")" "regression"
  check "regression container has no runs" "$(jq 'has("runs")' <<<"$body")" "false"
  check "regression cases hang off the container" \
    "$(jq -r --argjson id "$STAGE_ID" \
       '[.subTasks[] | select(.parentId == $id)] | length > 0' <<<"$ROOT_BODY")" "true"
  check "regression cases are real tasks with runs" \
    "$(jq -r --argjson id "$STAGE_ID" \
       '[.subTasks[] | select(.parentId == $id)
        | select((.virtual // false) == false and (.runId // 0) > 0)] | length > 0' <<<"$ROOT_BODY")" \
    "true"
fi

# The attempt's log: chunks after a cursor, with the attempt and the cursor
# echoed back so a viewer knows where to continue.
body_code=$(req GET "/api/tasks/$NODE_ID/log")
check "task log 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "task log defaults to the current attempt" \
  "$(jq -r .attempt <<<"$body")" "$NODE_ATTEMPT"
check "task log has chunks" "$(jq '.chunks | length > 0' <<<"$body")" "true"
LAST_SEQ=$(jq -r .lastSeq <<<"$body")
check "task log lastSeq is a cursor" "$([ "$LAST_SEQ" -gt 0 ] && echo yes)" "yes"

body_code=$(req GET "/api/tasks/$NODE_ID/log?after=$LAST_SEQ")
check "task log after lastSeq is empty" \
  "$(head -n1 <<<"$body_code" | jq '.chunks | length')" "0"

# An attempt that never logged anything is not an error: it reads as empty,
# with the attempt number echoed back.
body_code=$(req GET "/api/tasks/$NODE_ID/log?attempt=999")
check "task log unknown attempt echoed" \
  "$(head -n1 <<<"$body_code" | jq -r .attempt)" "999"
check "task log unknown attempt is empty" \
  "$(head -n1 <<<"$body_code" | jq '.chunks | length')" "0"

# The whole attempt as one text file, streamed chunk by chunk.
DL=$(curl -s -b "$CJAR" -o /tmp/api-smoke-log.txt -w '%{content_type}\n%{http_code}' \
  "$BASE_URL/api/tasks/$NODE_ID/log/download")
check "task log download 200" "$(tail -n1 <<<"$DL")" "200"
check "task log download is not empty" \
  "$([ -s /tmp/api-smoke-log.txt ] && echo nonempty)" "nonempty"
if grep -qi '^text/plain' <<<"$(head -n1 <<<"$DL")"; then
  check "task log download is text/plain" text text
else
  check "task log download is text/plain" "$(head -n1 <<<"$DL")" "text/plain"
fi

# A run detail: the attempt, every other attempt of the task, and what this
# one produced.
body_code=$(req GET "/api/test-runs/$NODE_RUN")
check "run detail 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "run detail attempt" "$(jq -r .attempt <<<"$body")" "$NODE_ATTEMPT"
check "run detail task" "$(jq -r .taskId <<<"$body")" "$NODE_ID"
check "run detail root" "$(jq -r .rootTaskId <<<"$body")" "$ROOT_ID"
check "run detail task key and kind" \
  "$(jq -r '"\(.taskKey)/\(.taskKind)"' <<<"$body")" "unit/unit"
check "run detail lists the attempts" \
  "$(jq -r '(.attempts | length) == .attempt' <<<"$body")" "true"
check "run detail lists artifacts" "$(jq '.artifacts | length > 0' <<<"$body")" "true"
ART_ID=$(jq -r '.artifacts[0].id' <<<"$body")

# The artifact itself: its metadata with the content, and the raw download.
body_code=$(req GET "/api/test-artifacts/$ART_ID")
check "artifact detail 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "artifact belongs to the run" "$(jq -r .runId <<<"$body")" "$NODE_RUN"
check "artifact content is served" "$(jq '.content | length > 0' <<<"$body")" "true"

DL=$(curl -s -b "$CJAR" -o /tmp/api-smoke-artifact.out -w '%{http_code}' \
  "$BASE_URL/api/test-artifacts/$ART_ID/download")
check "artifact download 200" "$DL" "200"
check "artifact download is not empty" \
  "$([ -s /tmp/api-smoke-artifact.out ] && echo nonempty)" "nonempty"

# The two bundles. A run's zip holds that attempt's own artifacts; a task's
# zip is the whole subtree, every descendant under a directory of its node
# key. Both are archives of their own ("PK"), and both are *complete* ones: the
# end-of-central-directory record is the last 22 bytes of a zip written in one
# piece, and a failed read mid-stream leaves the download without it.
ZIP=$(curl -s -b "$CJAR" -o /tmp/api-smoke-run.zip -w '%{http_code}' \
  "$BASE_URL/api/test-runs/$NODE_RUN/artifacts/zip")
check "run artifacts zip 200" "$ZIP" "200"
check "run artifacts zip is an archive" \
  "$([ "$(head -c2 /tmp/api-smoke-run.zip)" = "PK" ] && echo zip)" "zip"
check "run artifacts zip is complete" \
  "$([ "$(tail -c22 /tmp/api-smoke-run.zip | head -c4)" = $'PK\005\006' ] && echo eocd)" "eocd"

ZIP=$(curl -s -b "$CJAR" -o /tmp/api-smoke-task.zip -w '%{http_code}' \
  "$BASE_URL/api/tasks/$ROOT_ID/artifacts/zip")
check "task artifacts zip 200" "$ZIP" "200"
check "task artifacts zip is an archive" \
  "$([ "$(head -c2 /tmp/api-smoke-task.zip)" = "PK" ] && echo zip)" "zip"
check "task artifacts zip is complete" \
  "$([ "$(tail -c22 /tmp/api-smoke-task.zip | head -c4)" = $'PK\005\006' ] && echo eocd)" "eocd"

# A node whose latest attempt fetched nothing has no bundle: 404, because an
# empty archive would look like success. The clone stage declares no artifacts.
ZIP=$(curl -s -b "$CJAR" -o /tmp/api-smoke-clone.zip -w '%{http_code}' \
  "$BASE_URL/api/tasks/$CLONE_ID/artifacts/zip")
check "task zip without artifacts 404" "$ZIP" "404"

# ---------------------------------------------------------------------------
# 8d. Reporting an attempt: taskId is the whole address
# ---------------------------------------------------------------------------
echo "== reporting =="

# The body names the task; the run's environment, commit and kind are the
# task's own, so there is nothing to report without a taskId.
body_code=$(req POST /api/test-runs '{}')
check "report without taskId 400" "$(tail -n1 <<<"$body_code")" "400"

body_code=$(req POST /api/test-runs '{"taskId":999999,"status":"passed"}')
check "report unknown task 404" "$(tail -n1 <<<"$body_code")" "404"

# The status is one of the store's three, or absent (then it is derived from
# the counts).
body_code=$(req POST /api/test-runs "$(jq -n --argjson id "$NODE_ID" '{taskId: $id, status: "done"}')")
check "report bad status 400" "$(tail -n1 <<<"$body_code")" "400"

# A virtual node records no run: its status is the rollup of its children, so
# an attempt written against it would have nothing to aggregate into.
body_code=$(req POST /api/test-runs "$(jq -n --argjson id "$ROOT_ID" '{taskId: $id, status: "passed"}')")
check "report to a virtual task 400" "$(tail -n1 <<<"$body_code")" "400"
check "virtual report explains itself" \
  "$(head -n1 <<<"$body_code" | jq -r .error)" \
  "virtual tasks do not record runs; report against their children"

# The ownership rule, and the reason the report body no longer takes an
# environment: a run belongs to the environment its task ran on, and only that
# environment's owner (or an administrator) may write one. The demo graphs
# belong to the demo account; §2's second account is not an administrator, so
# every one of them is refused — and the refusal writes nothing.
body_code=$(req_other POST /api/test-runs "$(jq -n --argjson id "$NODE_ID" '{taskId: $id, status: "passed"}')")
check "report into a foreign environment 403" "$(tail -n1 <<<"$body_code")" "403"
check "foreign report changed nothing" \
  "$(req GET "/api/tasks/$NODE_ID/runs" | head -n1 | jq '.runs | length')" "$NODE_ATTEMPT"

# A report for a task whose attempt has already ended is a retry: the store
# opens the next attempt. That is what re-running a stage looks like.
body_code=$(req POST /api/test-runs "$(jq -n --argjson id "$NODE_ID" \
  '{taskId: $id, status: "passed", summary: "re-reported by the API smoke test", total: 4, passed: 4}')")
check "report attempt 201" "$(tail -n1 <<<"$body_code")" "201"
body="$(head -n1 <<<"$body_code")"
NEW_RUN=$(jq -r .id <<<"$body")
check "report opens the next attempt" "$(jq -r .attempt <<<"$body")" "$((NODE_ATTEMPT + 1))"
check "report keeps the task" "$(jq -r .taskId <<<"$body")" "$NODE_ID"
check "report takes the task's kind" "$(jq -r .kind <<<"$body")" "unit"
check "report takes the task's environment" "$(jq -r .environmentId <<<"$body")" "$CELL_ENV"
check "report takes the task's commit" "$(jq -r .commitId <<<"$body")" "$CELL_COMMIT"

body_code=$(req GET "/api/tasks/$NODE_ID")
check "task attempts incremented" \
  "$(head -n1 <<<"$body_code" | jq -r .attempts)" "$((NODE_ATTEMPT + 1))"
# The task detail carries the attempts themselves (newest first), not a
# runId — that is what the node list's entries carry.
check "task points at the new run" \
  "$(head -n1 <<<"$body_code" | jq -r '.runs[0].id')" "$NEW_RUN"

# Nothing was fetched back for the new attempt (a report carries no
# artifacts — those come from the stage), so its bundle is the 404 rule again.
ZIP=$(curl -s -b "$CJAR" -o /tmp/api-smoke-attempt.zip -w '%{http_code}' \
  "$BASE_URL/api/test-runs/$NEW_RUN/artifacts/zip")
check "new attempt zip 404" "$ZIP" "404"

# ---------------------------------------------------------------------------
# 8e. Jobs: manual trigger and monitoring
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

# The jobs list: one entry per root task, of every environment.
body_code=$(req GET /api/jobs)
check "jobs list 200" "$(tail -n1 <<<"$body_code")" "200"
check "jobs list is an array" \
  "$(head -n1 <<<"$body_code" | jq '.jobs | type == "array"')" "true"
check "jobs list carries the roots" \
  "$(head -n1 <<<"$body_code" | jq --argjson id "$ROOT_ID" '[.jobs[] | select(.id == $id)] | length')" "1"

# ---------------------------------------------------------------------------
# 8f. Dashboards: the matrix cell follows the reported attempt
# ---------------------------------------------------------------------------
echo "== test dashboard =="

# Unauthenticated dashboard access is rejected.
body_code=$(curl -s -w '\n%{http_code}' "$BASE_URL/api/dashboard/regression")
check "dashboard without session 401" "$(tail -n1 <<<"$body_code")" "401"

# Unknown kind and bad parameter are rejected.
body_code=$(req GET /api/dashboard/other)
check "dashboard unknown kind 404" "$(tail -n1 <<<"$body_code")" "404"

body_code=$(req GET "/api/dashboard/regression?commits=0")
check "dashboard commits=0 400" "$(tail -n1 <<<"$body_code")" "400"

# The unit matrix has a row per commit and a column per environment; the cell
# of the reported attempt is the run the report returned.
body_code=$(req GET "/api/dashboard/unit?commits=50")
check "dashboard unit 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "matrix lists the cell's environment" \
  "$(jq -r --argjson env "$CELL_ENV" \
     '[.environments[] | select(.id == $env)] | length' <<<"$body")" "1"
CELL_IX=$(jq -r --argjson env "$CELL_ENV" '[.environments[].id] | index($env)' <<<"$body")
ROW=$(jq -c --argjson cid "$CELL_COMMIT" \
  '[.rows[] | select(.commit.id == $cid)] | first // empty' <<<"$body")
if [ -z "$ROW" ]; then
  check "matrix row for the reported commit" missing present
else
  check "matrix row for the reported commit" found found
  check "matrix unit cell points at the new run" \
    "$(jq -r --argjson ix "$CELL_IX" '.cells[$ix].runId' <<<"$ROW")" "$NEW_RUN"
  check "matrix unit cell task" \
    "$(jq -r --argjson ix "$CELL_IX" '.cells[$ix].taskId' <<<"$ROW")" "$NODE_ID"
  check "matrix unit cell counts" \
    "$(jq -r --argjson ix "$CELL_IX" '.cells[$ix] | "\(.passed)/\(.total)"' <<<"$ROW")" "4/4"
fi

# The full matrix carries the same cell plus the stage list and the graph
# link, both keyed by environment id.
body_code=$(req GET "/api/dashboard/full?commits=50")
check "dashboard full 200" "$(tail -n1 <<<"$body_code")" "200"
body="$(head -n1 <<<"$body_code")"
check "full matrix links the graph" \
  "$(jq -r --argjson cid "$CELL_COMMIT" --arg env "$CELL_ENV" \
     '.rows[] | select(.commit.id == $cid) | .taskIds[$env]' <<<"$body")" "$ROOT_ID"
check "full unit stage points at the new run" \
  "$(jq -r --argjson cid "$CELL_COMMIT" --arg env "$CELL_ENV" \
     '.rows[] | select(.commit.id == $cid) | .stages[$env]
      | map(select(.kind == "unit")) | first | .runId' <<<"$body")" "$NEW_RUN"
check "full matrix column is the environment" \
  "$(jq -r --argjson env "$CELL_ENV" \
     '.environments[] | select(.id == $env) | .name | length > 0' <<<"$body")" "true"

# ---------------------------------------------------------------------------
# 9. Delete + logout
# ---------------------------------------------------------------------------
echo "== delete and logout =="

# The environment, its tasks, their runs and artifacts go together. There is
# no run to look up afterwards — a run needs a task and this environment never
# had one (reporting happens against a dispatched graph, §8b) — so what is
# checked here is that the delete reaches nothing else: not the other columns
# of the matrix, and not another account's graphs.
body_code=$(req DELETE "/api/environments/$ENV_ID")
check "delete environment 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(req GET "/api/environments/$ENV_ID")
check "get after delete 404" "$(tail -n1 <<<"$body_code")" "404"

# The matrix is looked up by environment id: the demo environments the seed
# wrote are still there, and they keep their columns.
body_code=$(req GET /api/dashboard/regression)
check "matrix env column gone after delete" \
  "$(head -n1 <<<"$body_code" | jq -r --argjson env "$ENV_ID" \
     '[.environments[] | select(.id == $env)] | length')" "0"
check "matrix keeps the other columns" \
  "$(head -n1 <<<"$body_code" | jq '.environments | length > 0')" "true"

check "other graphs survive the delete" \
  "$(req GET "/api/test-runs/$NODE_RUN" | tail -n1)" "200"

body_code=$(req POST /api/logout)
check "logout 200" "$(tail -n1 <<<"$body_code")" "200"

body_code=$(curl -s -b "$CJAR" -w '\n%{http_code}' "$BASE_URL/api/me")
check "me after logout 401" "$(tail -n1 <<<"$body_code")" "401"

# ---------------------------------------------------------------------------
echo
echo "passed: $PASS, failed: $FAIL"
[ "$FAIL" -eq 0 ]
