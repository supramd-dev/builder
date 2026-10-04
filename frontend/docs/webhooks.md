# GitLab webhooks

Point a GitLab project webhook (Settings → Webhooks) at:

```
POST https://your-server/api/webhooks/gitlab
```

with the **Push events**, **Tag push events** and **Merge request events**
triggers.

The endpoint cannot use a session cookie — the caller is the GitLab
server — so it authenticates with the site's webhook secret instead: copy
the value from the **Secret token** field of Settings → Webhook into the
webhook's own **Secret token** field in GitLab. GitLab then sends it back
with every event in the `X-Gitlab-Token` header, and an event without it
is rejected with `401` before the payload is parsed. The secret is
generated with the site configuration, so it is already there on a fresh
install; rotate it from the same tab if it leaks. See
[Site configuration → Webhook secret](#/docs/site-configuration).

## What an event does

Push, tag push and merge request events are recorded in the `commits` table
— each commit becomes a column of the test dashboard (deduplicated by
repo + sha; a re-recorded SHA restamps its row's event). Other event types
(pipeline, issue, …) are acknowledged with `status: ignored`.

| Event | Tested revision | Matrix row |
|---|---|---|
| **push** | head commit of the pushed list | event `push`, ref = branch |
| **tag push** | the tagged SHA (`after`) | event `tag_push`, ref = tag name (e.g. `v1.0`) |
| **merge request** | the MR's `last_commit` on the source branch | event `merge_request`, ref = source branch |

**Deletions are dropped.** A branch or tag deletion carries no commit to
test: GitLab puts the null SHA in `after` (and in a merge request's
`last_commit.id`), so the event is acknowledged with `status: ignored`
rather than recorded — a row nothing can ever fill would otherwise sit on
the matrix forever.

Merge request events dispatch on the `open`, `reopen` and `merge` actions
(a fresh source state or the merged result). Other actions (`update`,
`close`, `approved`, …) are recorded but not dispatched — the tested SHA
has not changed.

Each commit row stores which event created it (`event` column: `push`,
`tag_push`, `merge_request`, `manual`, `manual_yaml`); the dashboard shows
a small badge (tag / MR / M) next to the commit so manually and
webhook-triggered rows are distinguishable at a glance.

When the site config's **code repository** is set and the event targets
that repository (matched by path), dispatching kicks in automatically:

1. The server reads `md-builder.yaml` **at the event's commit**.
2. Matrix entries are matched to **enabled** environments by tags; one
   task graph (root + clone/build/test stages) is created per entry (see
   [Runner and tasks](#/docs/runner-strategy) and
   [The test matrix](#/docs/test-matrix)).
3. The response carries `jobsCreated` / `entriesSkipped`, plus a
   `dispatchError` when the YAML cannot be fetched or parsed — the
   commit is still recorded either way. It also carries
   `graphsCancelled`: the earlier graphs of that revision this event
   dropped under the `fork_cancel` policy, and 0 under the other two. A
   graph that had already finished, or that an earlier event had already
   dropped, is not counted — the number says what this event did. The
   response is for the caller (GitLab's webhook log); the same message is
   stored on the commit row, so the dashboard explains the commit's empty
   columns long after the response is gone — see below.

**GitLab gives a webhook ten seconds to answer**, so the read in step 1
must not scale with the repository. It does not: the server asks the code
host for that one file over its API — the same request `curl` would make:

```
GET /api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=<sha>
PRIVATE-TOKEN: <the Project Access Token>
```

One small request, the same cost whether the repository holds ten commits
or a hundred thousand.

When the file is not there at that commit — the usual case, when the YAML
has not been committed yet — the host says so, the dispatch ends with
`file not found — commit the md-builder.yaml to the repository root`, and
nothing else runs. That failure is the common one, so it is the one worth
keeping cheap.

Anything else the server cannot settle over that route falls back to a full
clone, which is slower and *can* exceed the ten seconds:

- the project is not readable with the configured token;
- the repository location has no usable https form (a bare `group/code`);
- the host answered with a page rather than the file (a sign-in redirect,
  say) — the server refuses to hand that to the YAML parser.

Nothing is lost either way: the commit is recorded before the fetch
starts, so the dashboard column appears immediately and its cells fill in
when the dispatch finishes. The fetch itself is capped (60 seconds by
default), so an unreachable repository server ends as a recorded
`dispatchError` rather than a request that never returns.

Note that GitLab's *web* route for a file — the `/-/raw/<ref>/<path>` URL a
browser opens — is not usable here: it authenticates through the browser
session only and ignores the token, answering with a redirect to the
sign-in page. The API route above is what accepts the token.

Nothing is dropped silently: every step between the event and the task
graphs leaves a record. A push that fails to dispatch (unreadable or
invalid `md-builder.yaml`, an entry matching no enabled environment)
shows a warning triangle in the **graph** column of the full dashboard —
hover for the reason, click for the full text. A push that is not
dispatched at all because the site has no code repository configured
says so on its row too. Failures *after* dispatch — the clone, the
build, the tests — are recorded on the tasks and runs themselves and
show up as the usual stage statuses.

Private repositories are handled with the Project Access Token
configured in the site settings (see
[Site configuration](#/docs/site-configuration)).

## Re-running dispatch

Task graphs are keyed by (commit, environment): pushing the same commit
again requeues its graph instead of duplicating it. To re-run a dispatch
without a push — after changing environment tags or the YAML — use the
jobs API with a session:

```
POST /api/jobs {"commitId": 7}
POST /api/jobs {"commitSha": "abc123", "commitRepo": "group/code"}
```

This re-reads the `md-builder.yaml` at that commit; the graphs are marked
as webhook-triggered, like the original push. To instead run a test with
**your own stage commands** (no YAML, any repository, any ref), use the
manual dispatch on the **Run command** page or
`POST /api/jobs/manual` — each manual dispatch gets its own matrix row
(see [Runner and tasks](#/docs/runner-strategy)).

`GET /api/jobs?limit=20` lists recent graphs for monitoring (status,
attempts, error).

### One SHA, two events: a push and then an MR

A common sequence triggers tests twice on one revision: you push a branch
(a **push** event), then open a merge request for it. The MR's
`last_commit` is the commit you just pushed, so both events carry the same
(repo, sha). What actually happens:

- **Both events dispatch.** The push dispatches, and the MR's `open`
  dispatches again — `open`, `reopen` and `merge` mean "a new source state
  to test", so the server does not try to recognise the second event as a
  repeat of the first. Two events, two dispatches.
- **What the second dispatch does to the first one's task is a setting.**
  **Settings → Dispatch** picks between requeueing that task (the default),
  giving the new event a task of its own, or doing that *and* cancelling the
  older one. The three modes are laid out in
  [Site configuration → Repeated commits](#/docs/site-configuration); what
  follows is how each one reads on the dashboard.

**Under the default (requeue):**

- **One column, not two.** `commits` is deduplicated by (repo, sha), so the
  MR re-records the row the push created: same row, same commit id, with
  `event` restamped to `merge_request` and `ref` to the MR's source branch
  (and the message to the MR's last-commit title). A commit you pushed can
  therefore wear an MR badge on the matrix — that is this restamp, not a
  second row. An MR row is *not* created on top of the pushed one, and the
  push's own `event` is not preserved.
- **The graph is the same graph.** Task graphs are keyed by
  (commit, environment), so the second dispatch requeues the graph the
  first one created instead of adding another: one root, one column of
  cells, `attempts` incremented.
- **A task that was still running starts over.** Requeuing re-arms every
  node as a fresh attempt — statuses and counters reset to `pending` — and
  closes the displaced in-flight attempt's run as `skipped` with the
  summary `superseded by a new dispatch of this task`; nothing else would
  ever close it. The stage then runs again from the beginning on the new
  attempt, while the superseded attempt, its log and its artifacts stay
  readable on the run page as history.

So "the push's task got relabelled MR and restarted" is the designed
behaviour rather than a lost result: one commit row, one graph, with the
new attempt running and the earlier one kept as history. Note that the
restamp is independent of the dispatch — an MR action that does *not*
dispatch (`update`, `close`, `approved`) still restamps the row's event
and ref, because the SHA is the same revision either way. The same holds
for any other re-recording of an already-recorded SHA: a tag push of a
pushed commit, or re-running a manual yaml dispatch of the same ref.

**Under `fork`:** the MR gets a commit row and a task graph of its own, and
the push's are left exactly as they were — still queued or still running,
nothing restamped, nothing closed. The matrix then shows two rows for that
SHA, each with its own cells; the older one is marked `older` and dimmed
once it stops moving. Both graphs really do run at the same time, on the
same environment: expect the load, and expect two sets of logs.

**Under `fork_cancel`:** the same, plus the older rows' unfinished work is
cancelled once the new graphs exist — a stage still running on an
environment is aborted, its attempt is closed as `cancelled` with the
summary `cancelled by a newer dispatch of this commit`, and a stage that had
already finished keeps its result and its log. The response then carries
`graphsCancelled` next to `jobsCreated`: how many of the revision's earlier
graphs this event dropped. A dispatch that creates nothing — an unreadable
YAML, no entry matching an environment — cancels nothing: the cancellation
waits for the new work to exist, so a broken read never kills a running
test.
