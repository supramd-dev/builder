# The test matrix (md-builder.yaml)

Place a md-builder.yaml at the **root of the code repository** — it
changes with the code, and a push that changes it changes the dispatch.
On every push the server reads it at the pushed commit (git show
<sha>:md-builder.yaml), so matrix changes take effect on the commit
that introduces them.

A fully commented, copy-paste starting point lives in the md-builder
source tree as
[md-builder.example.yaml](https://github.com/genshen/md-builder/blob/main/md-builder.example.yaml).

## Full example

```yaml
version: 2

# Optional defaults, merged into every matrix entry (maps merge key-wise,
# scalars are overridden per entry).
defaults:
  timeout: 3600                 # per-command timeout seconds (hard cap 4h)
  env:
    OMP_NUM_THREADS: "4"
  build:
    # A plain shell command — cmake/make/script, whatever the project uses.
    command: "cmake -DCMAKE_BUILD_TYPE=Release . && cmake --build . -j8"

# Regression presets: shared test cases referenced by matrix entries.
# Each preset is one regression case — how to run it and which artifact
# files to collect.
presets:
  heat:
    description: Heat equation convergence
    command: "python3 run_heat.py"
    workdir: "regression/heat"        # relative to the code directory
    timeout: 1800
    artifacts: "out.xml"
  poisson:
    command: "python3 run_poisson.py --nt 200"
    artifacts: ["poisson.xml", "poisson.log"]

# Required: the matrix. Each entry names the tags an environment must
# carry; dispatch picks one matching enabled environment per entry.
matrix:
  - tags: [cpu]
    description: Generic CPU build
    env:
      CC: gcc
      CXX: g++
    build:
      command: "cmake -DENABLE_MPI=OFF . && cmake --build ."
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      timeout: 600
      artifacts: "build/test_detail.xml"  # googletest XML file
    regression:
      use: [heat, poisson]          # which presets run here (empty = all)
  - tags: [gpu, cuda]
    env:
      CC: clang
    build:
      command: "./build.sh --cuda"   # any build tool, not just cmake
    unit:
      command: "ctest --test-dir build -L unit"
    regression:
      disable: [heat]           # run every preset except heat
```

## Field reference

| Field                        | Required | Description                                                        |
|------------------------------|----------|--------------------------------------------------------------------|
| version                      | yes      | Must be 2.                                                          |
| defaults                     | no       | Entry-level defaults: timeout, env, build, unit, regression.        |
| presets                      | no       | Shared regression cases (see below).                               |
| matrix                       | yes      | One or more entries; each entry needs tags and at least one stage.  |
| matrix[].tags                | yes      | Tags selecting the environment (see [Test environments](#/docs/environments)). Must be unique per entry. |
| matrix[].description         | no       | Human-readable label.                                               |
| matrix[].timeout             | no       | Default stage timeout in seconds (default 3600, capped at 14400).   |
| matrix[].env                 | no       | Extra environment variables exported for all stages.                |
| matrix[].build               | no       | Build stage (see below).                                            |
| matrix[].unit                | no       | Unit test stage: at least command; optional workdir, timeout, artifacts. |
| matrix[].regression          | no       | Regression selection: use and/or disable referencing presets.       |
| unit.command                 | yes (per stage) | Shell command, or a list of commands (see Command lists).    |
| unit.workdir                 | no       | Directory the command runs in (see Working directories).            |
| unit.artifacts               | no       | Artifact file path (or list) the runner fetches back (see Artifact files). |
| unit.timeout                 | no       | Stage timeout overriding defaults.                                  |

The build stage is one shell command (no built-in cmake support — write
the cmake/make/ninja/script invocation yourself):

| Field          | Description                                                          |
|----------------|----------------------------------------------------------------------|
| build.command  | Shell command compiling the code (required — entry or defaults).     |
| build.workdir  | Directory the command runs in (same semantics as unit/presets).      |

Example — out-of-source cmake via the workdir and the built-in
`$MD_CODE_DIR` variable:

```yaml
build:
  command: 'cmake "$MD_CODE_DIR" -DCMAKE_BUILD_TYPE=Release && cmake --build . -j16'
  workdir: "build"   # runs in <code>/build
```

Every timeout bounds the stage via the remote `timeout` command; the
whole SSH session gets the sum of the stage timeouts plus 15 minutes of
slack.

## Regression presets

Regression tests are defined **once**, in the top-level `presets` map,
and referenced from each matrix entry — the same case does not need to
be repeated per platform:

```yaml
presets:
  heat:
    description: Heat equation convergence
    command: "python3 run_heat.py"
    workdir: "regression/heat"
    timeout: 1800
    artifacts: "out.xml"
```

| Field                | Required | Description                                                   |
|----------------------|----------|---------------------------------------------------------------|
| presets.<name>.command | yes    | Shell command, or a list of commands (see Command lists).    |
| presets.<name>.description | no | Human-readable label.                                         |
| presets.<name>.workdir | no    | Directory the command runs in (see Working directories).     |
| presets.<name>.timeout | no     | Case timeout (falls back to defaults/matrix timeout).        |
| presets.<name>.artifacts | no    | Artifact files to collect (see Artifact files).              |

Each referenced preset becomes its **own sub-task** in the task graph
(“regression: heat”), which runs after the build with its own timeout,
its own log and its own child test run under the parent regression run.
The cases run independently — one failing case does not stop the others —
and the matrix cell aggregates all cases of the entry.

A matrix entry selects presets with `regression.use` and
`regression.disable`:

- **use**: the list of preset names to run. When omitted or empty,
  **every preset runs** (in name order).
- **disable**: preset names removed from the used set — handy with an
  empty use (“everything except poisson”).
- Names in `use` or `disable` that do not exist in `presets` fail
  validation.

### Pass / fail of a case

A case's verdict is its **command's exit status** — nothing else:

- **exit 0 → passed**; any non-zero exit → failed. That includes the
  timeout (the runner wraps the command in the remote `timeout`, which
  exits 124) and a failed `cd` into the workdir.
- An SSH-level failure (host unreachable, session dropped) fails the
  case the same way, with the transport error as the case's note.
- The preset's `artifacts` files **never flip the verdict** — they are
  stored as artifacts of the case's own child run (per-case detail parsed
  in the browser). This differs from the unit stage, where artifact files
  reporting failed cases also fail the run.
- An `MD-BUILDER-SUMMARY:` line only becomes the case's note; it cannot
  turn a non-zero exit into a pass.

The entry's regression run (the matrix cell) aggregates its cases: any
failed case → the cell shows ✗, every case passed → ✓. Cases run
independently — one failing case does not stop the others. When the
clone or build fails, every case is recorded as **skipped** (⤼) with the
upstream error as its note.

## Command lists

The `command` of a unit stage or a regression preset may be **one
command or a list**:

```yaml
command: "make data && ctest -L unit"       # scalar: one bash -c line
command: ["make data", "ctest -L unit"]     # list: two commands
command: |                                  # block scalar: still one command
  ./configure --enable-mpi
  make -j8
```

- The **scalar** form is a single `bash -c` invocation — chain with `&&`,
  `;`, pipes or a block scalar however you like.
- The **list** form runs the entries in order, each under its own
  `timeout` wrapper, chained with `&&`: **a failing command stops the
  stage right there** — the remaining commands do not run, and the case
  fails with that command's exit code.
- Blank entries are dropped (handy with block scalars split into lines).

The list form is exactly equivalent to joining with `&&` in one string;
it just reads better (and keeps `&&` out of quoted yaml).

## Built-in environment variables

Every stage script (build, unit, regression case) exports these before
the stage command runs — the commands can rely on them:

| Variable       | Meaning                                                        |
|----------------|----------------------------------------------------------------|
| MD_COMMIT      | Full commit SHA under test.                                     |
| MD_ENV_NAME    | Name of the environment the stage runs on.                      |
| MD_ENV_TAGS    | Comma-separated tags of that environment.                       |
| MD_TASK_DIR    | Remote task directory (~/.md-builder/tasks/<sha12>).            |
| MD_CODE_DIR    | Code directory — MD_TASK_DIR/code.                              |
| MD_CASE        | Case name (regression case scripts only).                       |

The yaml `env` variables are exported right after the MD_* variables,
so a command can override neither (they are exported earlier — see the
env setup script below for the override point).

```yaml
unit:
  command: "$MD_CODE_DIR/build/unit_tests --gtest_output=xml:$MD_CODE_DIR/build/test_detail.xml"
```

## Environment setup script

Each **environment** (configured on the site, not in the yaml) may
carry an env setup script — the script content is edited in the
environment settings form and stored on the server. On dispatch it is
written to the task dir as `md-builder-env-<hash>.sh` and **sourced by
every stage script** before the stage command (module loads, compiler
exports, virtualenv activation, …):

- **present**: every stage script runs `. md-builder-env-<hash>.sh`
  first; anything the script exports is visible to the build, unit and
  regression commands (it runs last in the preamble, so it can even
  override the built-in and yaml variables).
- **absent**: the stage scripts log a warning and run without it.

See [Test environments](#/docs/environments).

## Working directories

`build.workdir`, `unit.workdir` and `presets.<name>.workdir` set the
directory a stage command runs in:

- **empty** (default): the code directory (MD_CODE_DIR).
- **relative**: MD_CODE_DIR/<workdir> — the directory must exist in the
  repository (the runner does not create it).
- **absolute**: used as-is on the remote host.

An out-of-source build is just a `workdir` plus a command that
configures against `"$MD_CODE_DIR"` (see the build example above).

## Artifact files

A stage command (unit or a regression preset) may produce structured
result files — by default the googletest XML (`--gtest_output=xml:`)
or JSON (`--gtest_output=json:`) format, which the test binary writes
itself. A run can produce several of them; `artifacts` accepts a single
path or a list:

```yaml
unit:
  command: "./build/unit_tests --gtest_output=xml:build/test_detail.xml"
  artifacts: "build/test_detail.xml"
```

```yaml
unit:
  command: "ctest --output-junit junit.xml && ./build/extra_tests --gtest_output=json:build/extra.json"
  artifacts: ["build/test_detail.xml", "build/extra.json"]
```

When `artifacts` is set, the runner reads every configured file back after
the command and:

- stores each one **verbatim** as its own run artifact, and
- extracts only the aggregate counts (total / failed / skipped) from each
  file's root attributes, **summed across all files**, for the matrix cell.

The per-case list — name, status, duration, failure message — is parsed
**in the browser** when you open the run's detail page; the server never
interprets individual cases. Every stored artifact file is parsed by its
default name/format; files that are missing or unrecognized are skipped
(noted in the stage log) and do not fail the run — it still records the
command's exit status. Paths are relative to the stage's working
directory (absolute paths work too).

For the **unit** stage the parsed counts matter to the verdict: failed
cases in the artifact file fail the run even when the command exited zero
(ctest-style wrappers can swallow the test binary's exit code). For
**regression presets** the files are display-only — the case's verdict is
its command's exit status (see Pass / fail of a case).

## Validation rules

- `version` must be 2; `matrix` must be non-empty.
- Each entry needs non-empty `tags` and at least one of `unit` /
  `regression` (or its presets expansion), with a `command`.
- Duplicate tag sets across entries are rejected.
- `build.command` is required (the entry's own or `defaults.build`).
- Unknown fields are rejected (e.g. the removed `build.generator` /
  `cmake_flags` / `threads`).
- Every preset needs a `command`; names in `use` / `disable` must
  reference defined presets.

Invalid YAML fails dispatch: the push is recorded and
`dispatchError` surfaces in the webhook response (see
[Webhooks](#/docs/webhooks)), but no tasks are created.

## What the runner does

Each matched entry becomes a task graph (see
[Runner and tasks](#/docs/runner-strategy)); the stages run in order:

1. **clone**: the *server* clones the code repository at the pushed
   commit, packs the working tree into a tarball and extracts it on the
   environment into ~/.md-builder/tasks/<sha12>/code. The env setup
   script is written into the task dir.
2. **build**: a generated script exports MD_COMMIT, MD_ENV_NAME,
   MD_ENV_TAGS, MD_TASK_DIR, MD_CODE_DIR plus the yaml env variables,
   sources the env setup script (if any) and runs the build stage in
   its working directory.
3. If the build (or the clone) fails, the dependent test stages are
   marked skipped and the dashboard shows ✗.
4. **unit**: the stage command runs in its working directory under its
   timeout; the full output streams into the task log and the outcome
   is stored as a test run.
5. **regression: one sub-task per selected preset** — each case command
   runs after the build (exporting MD_CASE), collects its own artifact
   files and records its own child test run; the matrix cell shows the
   aggregate across cases.

## Custom summaries

A stage command may print a summary line on stdout:

```
MD-BUILDER-SUMMARY: all 8 tests passed, max rel err 3.2e-7
```

The text after the prefix becomes the run summary shown on the dashboard.
Without it, the summary is the exit code plus the last lines of the stage
log (truncated to 500 characters). For a regression case the summary
line becomes the child run's note.
