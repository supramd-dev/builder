# Site configuration

Open **Settings** and fill in:

- **Code repository** — the repository under test. Webhook pushes to it
  trigger test runs. The md-builder.yaml is read from this repository.
  Test inputs are expected to live inside the code repository itself
  (or to be fetched by it) — there is no separate test-input repository.

Repositories are expected to be hosted on **GitLab** (gitlab.com or any
self-hosted instance). Locations are not host-validated, so use the URL
form of your instance:

```
https://gitlab.example.com/group/code
git@gitlab.example.com:group/code.git
```

## Credentials for private repositories

For private repositories configure a **Project Access Token** — created in
GitLab under *Settings → Access Tokens* with the `read_repository` scope.
The token is used by the **server only**: to read the test matrix from the
code repository and to clone it before uploading it to the test
environments (the environments themselves need no repository access —
see [Runner and tasks](#/docs/runner-strategy)). Repository locations
given in SSH form (ssh:// or git@host:group/repo) are cloned over https
with the token. For public repositories leave the token empty.

The token is write-only: the form shows whether one is configured, never
the value itself; leave the field blank to keep the stored token, tick
the *Remove* checkbox to clear it.

## Display timezone

The **Settings → Display** tab sets the timezone every timestamp is
displayed in (dashboards, task and run pages): pick an IANA zone such as
`Asia/Shanghai`, or leave it on *Browser local* so each viewer sees times
in their own zone. The setting is display-only — stored data and logs keep
their original timestamps, and the browser caches the choice locally so
pages render immediately after a reload.
