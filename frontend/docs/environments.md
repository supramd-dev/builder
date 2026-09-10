# Test environments

Register the machines that run the tests in the **User center**: name,
SSH host, SSH username and an SSH private key. Use **Test** to verify
connectivity, **Run command** to try commands interactively.

The server connects over SSH to upload the sources (a tar stream extracted
into `~/.md-builder/tasks/<sha12>`) and to run the build/test scripts — see
[Runner and tasks](#/docs/runner-strategy).

## Prerequisites

Each environment needs:

- `bash`, `tar`, `gzip` and `timeout` (coreutils) — used by every task.
- The toolchain the build and test commands use (cmake, compilers,
  python, …), installed however the machine's admins prefer.

The environment needs **no git** and no access to the repositories: the
server clones the code and the test inputs itself and ships the working
trees over.

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
