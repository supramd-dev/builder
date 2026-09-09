# GitLab webhooks

Point a GitLab project webhook (Settings → Webhooks) at:

```
POST https://your-server/api/webhooks/gitlab
```

with the **Push events** trigger. The endpoint is unauthenticated (it is
called by the GitLab server); verify the `X-Gitlab-Token` header once a
secret is configured.

## What a push does

Every push event is recorded in the `commits` table — each commit
becomes a column of the test dashboard (deduplicated by repo + sha).
Other event types (pipeline, tag push, …) are acknowledged with
`status: ignored`.

When the site config's **code repository** is set and the push targets
that repository (matched by path), dispatching kicks in automatically:

1. The server reads `md-builder.yaml` **at the pushed commit**.
2. Matrix entries are matched to **enabled** environments by tags; one
   job is created per entry (see
   [The test matrix](#/docs/test-matrix)).
3. The response carries `jobsCreated` / `entriesSkipped`, plus a
   `dispatchError` when the YAML cannot be fetched or parsed — the
   commit is still recorded either way.

Private repositories are handled with the deploy key / deploy token
configured in the site settings (see
[Site configuration](#/docs/site-configuration)).

## Re-running dispatch

Jobs are keyed by (commit, environment): pushing the same commit again
requeues its jobs instead of duplicating them. To re-run a dispatch
without a push — after changing environment tags or the YAML — use the
jobs API with a session:

```
POST /api/jobs {"commitId": 7}
POST /api/jobs {"commitSha": "abc123", "commitRepo": "group/code"}
```

`GET /api/jobs?limit=20` lists recent jobs for monitoring (status,
attempts, error).
