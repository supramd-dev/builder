# Site configuration

Open **Settings** and fill in:

- **Code repository** — the repository under test. Webhook pushes to it
  trigger test runs. The md-builder.yaml is read from this repository.
- **Test input repository** — the repository holding the test inputs
  (test cases) the code is exercised with.
- **Test input branch or commit id** — the ref of the test input
  repository to clone during runs.

Repositories are expected to be hosted on **GitLab** (gitlab.com or any
self-hosted instance). Locations are not host-validated, so use the URL
form of your instance:

```
https://gitlab.example.com/group/code
git@gitlab.example.com:group/code.git
```

## Credentials for private repositories

For private repositories configure **one** of:

- **Deploy token** — a GitLab deploy token or personal/group access token
  with the read_repository scope, plus the username shown next to it in
  GitLab (e.g. gitlab+deploy-token-42; leave the username empty for the
  default oauth2). Applies to https repository URLs.
- **Deploy key** — a PEM-encoded SSH private key whose public half is
  registered as a GitLab deploy key with read access to both
  repositories. https URLs are converted to their ssh:// form
  automatically.

Both are used in two places: by the server (to read the test matrix from
the code repository) and by the generated job scripts on the test
environments (to clone both repositories). Secrets are write-only: the
form shows whether one is configured, never the value itself; leave the
field blank to keep the stored secret, tick the *Remove* checkbox to
clear it.
