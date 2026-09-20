# Site configuration

On a site that has no account yet, the code repository is the first block of
the setup page that opens instead of the login form (see
[Getting started](#/docs/getting-started)) — the same two fields, stored in
the same place. Everything below is the **Settings** page, where they are
changed afterwards.

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
That one scope covers both things the server does with the token: read the
test matrix from the code repository (see
[GitLab webhooks](#/docs/webhooks)) and clone it before uploading it to
the test environments (the environments themselves need no repository
access — see [Runner and tasks](#/docs/runner-strategy)). Repository
locations given in SSH form (ssh:// or git@host:group/repo) are read and
cloned over https with the token. For public repositories leave the token
empty.

The token is write-only: the form shows whether one is configured, never
the value itself; leave the field blank to keep the stored token, tick
the *Remove* checkbox to clear it.

## Secret token for commands

The **Settings → Repository** tab also carries an optional **secret
token** — a site-wide secret exported to every stage command
(`build.command`, `unit.command`, regression preset commands in
md-builder.yaml) as the environment variable `MD_SECRET_TOKEN`. It lets
those commands authenticate against internal services — package mirrors,
artifact stores, licensed-software license servers — without hardcoding
credentials in the code repository.

```yaml
build:
  command: "cmake -DFETCH_TOKEN=\"$MD_SECRET_TOKEN\" . && cmake --build ."
```

The same write-only convention applies: only whether it is set is ever
reported. If a command echoes the value into its output (`env`,
`set -x`, `curl -v`), the runner replaces every occurrence with
`REDACTED` in the task log before storing it. See
[Test matrix → Secret token](#/docs/test-matrix) for usage.

## Webhook secret

The **Settings → Webhook** tab shows the shared secret the webhook
endpoint verifies. It is generated automatically the first time the site
configuration is read — no setup step — and every webhook event GitLab
delivers has to carry it back in the `X-Gitlab-Token` header. An event
with a missing or wrong token is rejected with `401` before its body is
parsed, so a forged push cannot record a commit or start a build.

Paste the value into the webhook's **Secret token** field on the GitLab
side. **Regenerate** replaces it with a fresh random value; the GitLab
webhook keeps failing until the new value is saved there, so rotate it
when the old one may have leaked (a screenshot, a shared terminal, a
GitLab export).

This secret and the secret token above are two different things, going in
opposite directions:

| | Direction | Purpose | Visible in the UI |
|---|---|---|---|
| **Webhook secret** (this tab) | inbound | authenticates GitLab's calls to md-builder | yes, to administrators |
| **Secret token** (Repository tab) | outbound | authenticates your build commands against internal services, as `MD_SECRET_TOKEN` | never |

Reading or rotating the webhook secret requires an administrator account;
a regular user opening the tab sees the webhook URL and a note to ask an
administrator for the token. See
[User accounts](#/docs/site-configuration) for the two roles.

## Display timezone

The **Settings → Display** tab sets the timezone every timestamp is
displayed in (dashboards, task and run pages): pick an IANA zone such as
`Asia/Shanghai`, or leave it on *Browser local* so each viewer sees times
in their own zone. The setting is display-only — stored data and logs keep
their original timestamps, and the browser caches the choice locally so
pages render immediately after a reload.

## GitLab sign-in

The **Settings → GitLab** tab (administrators only) lets people sign in with
their GitLab account instead of a local password. It is independent of the
repository configuration above: that token reads the *code* under test,
whereas this one identifies *people* — a site can clone from one GitLab
instance and authenticate against another.

On the GitLab instance, create an application with the **`read_user`** scope
(on gitlab.com under *Preferences → Applications*, on a self-managed instance
under *Admin Area → Applications*) and give it the **Redirect URI** the tab
shows. Then fill in the tab:

| Field | What it is |
| --- | --- |
| GitLab site address | The instance users sign in against, e.g. `https://gitlab.com`. A full URL, scheme included — a bare host is refused. |
| Redirect URI | Read-only. Built by the server from `server.publicURL` — copy it into the application as-is. |
| Application ID | The application's *Application ID*. |
| Application secret | The application's *Secret*. Write-only, like the repository token: stored server-side, never shown again. |
| Offer sign-in with GitLab | The switch. While it is off, only local accounts can sign in. |

The Redirect URI comes from the server configuration, not from the request:

```yaml
server:
  # The address users reach this site at. Required for GitLab sign-in: the
  # callback URL is built from it and must match the redirect URI registered
  # on the GitLab application.
  publicURL: https://md.example.com
```

It is a configuration value on purpose. Deriving it from the request's `Host`
header would let a caller point the callback at a server of their choosing,
and the value has to match what was registered on GitLab anyway. Like the
GitLab address it has to be a full URL with the scheme: without one the
callback address would come out relative, which GitLab rejects with an error
that says nothing about the cause.

Switching the integration on with a field missing is refused — a site would
otherwise advertise a button that cannot work. Leaving the address or the
application id blank in a later save is not the same as clearing it: a blank
field means "keep what is stored", so flipping only the switch leaves the
credentials alone. Clearing the application secret has its own checkbox, and
clearing the address or the id while the integration is on is refused for the
same reason as above.

### What happens on a first sign-in

A GitLab sign-in **registers** an account; it does not admit one. The new
account is created as a regular user (never an administrator, whatever GitLab
says), with no password, and lands in the **Settings → Users** tab marked
**Pending approval**. Until an administrator presses **Approve** it cannot
sign in, by GitLab or by password — the login page says the account is
awaiting approval. The Accounts table carries a notice at the top while
anything is waiting, and the **Source** column shows **GitLab** or **Local**,
so a self-registration is visible at a glance.

An account's identity is its GitLab user id, so later sign-ins land on the
same account and no second one is created. The GitLab username is only a
label: if it collides with an existing one, the new account gets a numeric
suffix (`alice`, `alice-2`, …).

Two things are refused rather than resolved automatically:

- **An email address that already belongs to an account here.** GitLab's idea
  of who owns an address is not something this site can verify, so linking the
  two would hand an existing account to whoever controls that address on the
  GitLab instance. The sign-in is refused with a message; the existing account
  is untouched.
- **A GitLab account with no visible email.** There is nothing to build an
  account on, so GitLab sign-in cannot be used for it.

Disabling an approved account keeps it out of the GitLab path too, with the
same message as a password login, and **un-approving** one does the same
thing: it goes back to awaiting approval. Both decisions end the account's
live sessions immediately, so access stops when the decision is made rather
than when the session happens to expire.

## User accounts

Accounts come in two kinds. A **regular user** signs in and uses md-builder.
An **administrator** additionally manages the accounts, in the
**Settings → Users** tab: every account with its username, email, source,
role, status and creation date, an **Edit** action (username, email,
password) and a **Disable**/**Enable** action. A regular user sees the same
tab under the name **Account**, holding their own details and nothing else.

An account's **source** is `Local` (created here, by an administrator or the
first-run setup) or `GitLab` (registered by a GitLab sign-in, see
[GitLab sign-in](#gitlab-sign-in) above). It is a fact about the account, not
a setting, and no form can change it.

Administrators are created on the server, and nowhere else:

```sh
md-builder adduser -admin -username root -email root@example.com
```

No request the web UI can make creates or promotes an administrator — a role
is not part of any account form — so the panel is not a way to acquire one.

**Disabling** an account signs it out at once and refuses further logins
("this account has been disabled"). The account and everything it configured
stay in place; **Enable** restores access. **Approving** admits an account
that registered itself through GitLab, and withdrawing that approval sends it
back to awaiting it; both sign the account out at once, for the same reason
disabling does — see [GitLab sign-in](#gitlab-sign-in). You cannot disable or
approve your own
account, nor another administrator's, so a site cannot be left with no way in.

Changing a password signs that account out everywhere except the browser that
made the change — which is what makes a reset a reset. Passwords must be at
least 8 characters; usernames and email addresses must be unique.

## Object storage

The settings above live in the database and are edited in the browser.
One deployment setting does not: the **object storage** the test output
files are kept in. It is configured on the server host, in
`md-builder-server.yaml` or through `MD_BUILDER_S3_*` environment
variables, and the server will not start without it. See
[Object storage (MinIO)](#/docs/object-storage).
