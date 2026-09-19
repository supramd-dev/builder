# Test environments

Register the machines that run the tests under **Runner Envs**: name,
SSH host, SSH username, an SSH private key and an environment setup
script. Use **Test** to verify connectivity, **Run command** to try
commands interactively or dispatch a manual test on the machine (see
[Runner and tasks](#/docs/runner-strategy)).

The server connects over SSH to upload the sources (a tar stream extracted
into `~/.md-builder/tasks/<sha12>`) and to run the build/test scripts — see
[Runner and tasks](#/docs/runner-strategy).

## Who may see and change an environment

Environments are a **shared, site-wide pool**. A push is dispatched to
whichever environment's tags match an entry in `md-builder.yaml`, no matter
which account registered that machine — so every signed-in user sees every
environment in the list and every environment is a column of the dashboard
matrix (which labels each column with its owner).

What differs is what you may do with a row:

| | Owner | Administrator | Everyone else |
|---|---|---|---|
| See it in the list and on the dashboard | yes | yes | yes |
| Edit, enable/disable, delete | yes | yes | no (read-only) |
| **Test**, **Run command**, **Run script** (uses the stored private key) | yes | yes | no |

A refused write is `403` with a message naming the reason; a row you do not
own shows as read-only instead of offering buttons that would fail. The
owner is the account that created the environment, and it never changes.

Administrators can therefore clean up an environment nobody maintains — for
instance the demo rows left behind by `md-builder seed` (user `demo`), which
a normal user could neither disable nor delete.

## Prerequisites

Each environment needs:

- `bash`, `tar`, `gzip` and `timeout` (coreutils) — used by every task.
- The toolchain the build and test commands use (cmake, compilers,
  python, …), installed however the machine's admins prefer.

The environment needs **no git** and no access to the repositories: the
server clones the code repository itself and ships the working tree
over.

## Tags

Every environment carries one or more **tags** (lowercase words like cpu,
gpu, cuda, mpi). Tags are how the test matrix selects environments: a
matrix entry lists the tags it requires and md-builder picks *one*
environment whose tags include all of them. Rules:

- Matching is a superset match, case-insensitive: an environment with
  tags cpu, mpi satisfies an entry asking for cpu.
- Extra unrelated tags do not prevent a match; among several matches the
  "most compact" environment wins (fewest extra tags, then name order).
- Each entry runs on exactly one environment; if no environment matches,
  the entry is skipped (counted as entriesSkipped in the dispatch
  response, not an error).
- Environments must be **enabled** to receive tasks; disabled ones stay
  greyed out on the dashboard.

## Environment setup script

Each environment may carry a bash **env setup script** (edited in the
environment form). On dispatch it is written into the task dir as
`md-builder-env-<hash>.sh` (the hash is derived from the content, so
editing the script changes the file name) and **sourced by every stage
script** — build, unit test and each regression case — before the stage
command runs:

```bash
module load gcc/13 openmpi/4.1
export CXX=mpicxx
source /opt/profiles/intel.sh
```

Use it for module loads, compiler exports, virtualenv activation —
anything the stage commands assume. Because it runs last in the script
preamble, it can even override the built-in and yaml variables (see
[Built-in environment variables](#/docs/test-matrix)).

An environment without a script is fine: stage scripts log a warning
(`env script ... not found`) and run without it.
