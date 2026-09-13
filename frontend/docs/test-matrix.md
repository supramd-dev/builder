# The test matrix (md-builder.yaml)

Place a md-builder.yaml at the **root of the code repository** — it
changes with the code, and a push that changes it changes the dispatch.
On every push the server reads it at the pushed commit (git show
<sha>:md-builder.yaml), so matrix changes take effect on the commit
that introduces them.

## Full example

```yaml
version: 1

# Optional defaults, merged into every matrix entry (maps merge key-wise,
# scalars are overridden per entry).
defaults:
  timeout: 3600                 # per-command timeout seconds (hard cap 4h)
  env:
    OMP_NUM_THREADS: "4"
  build:
    generator: cmake            # cmake (default) | script
    cmake_flags: "-DCMAKE_BUILD_TYPE=Release"
    threads: 8                  # cmake --build -j

# Required: the matrix. Each entry names the tags an environment must
# carry; dispatch picks one matching enabled environment per entry.
matrix:
  - tags: [cpu]
    description: Generic CPU build
    env:
      CC: gcc
      CXX: g++
    build:
      cmake_flags: "-DENABLE_MPI=OFF"
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      timeout: 600
      results: "build/test_detail.xml"   # googletest results file
    regression:
      command: "python3 run_regression.py --suite full"
      timeout: 1800

  - tags: [gpu, cuda]
    env:
      CC: clang
    build:
      generator: script         # non-cmake projects
      command: "./build.sh --cuda"
    unit:
      command: "ctest --test-dir build -L unit"
```

## Field reference

| Field                        | Required | Description                                                        |
|------------------------------|----------|--------------------------------------------------------------------|
| version                      | yes      | Must be 1.                                                          |
| defaults                     | no       | Entry-level defaults: timeout, env, build, unit, regression.        |
| matrix                       | yes      | One or more entries; each entry needs tags and at least one stage.  |
| matrix[].tags                | yes      | Tags selecting the environment (see [Test environments](#/docs/environments)). Must be unique per entry. |
| matrix[].description         | no       | Human-readable label.                                               |
| matrix[].timeout             | no       | Default stage timeout in seconds (default 3600, capped at 14400).   |
| matrix[].env                 | no       | Extra environment variables exported for all stages.                |
| matrix[].build               | no       | Build stage (see below).                                            |
| matrix[].unit                | no       | Unit test stage: at least command; optional timeout, results.        |
| matrix[].regression          | no       | Regression test stage: at least command; optional timeout, results.  |
| unit.command / regression.command | yes (per stage) | Shell command run in the code directory.                    |
| unit.results / regression.results | no | Results file path (or list of paths) the runner fetches back (see Results files). |

The build stage has two forms:

| Field               | Description                                                                 |
|---------------------|-----------------------------------------------------------------------------|
| build.generator     | cmake (default) or script.                                                  |
| build.cmake_flags   | Flags passed to cmake (cmake generator only).                               |
| build.threads       | Parallel build jobs (default 8).                                            |

| build.command       | Shell command (script generator only).                                      |

Every timeout bounds the stage via the remote `timeout` command; the
whole SSH session gets the sum of the stage timeouts plus 15 minutes of
slack.

## Validation rules

- `version` must be 1; `matrix` must be non-empty.
- Each entry needs non-empty `tags` and at least one of `unit` /
  `regression`, each with a `command`.
- Duplicate tag sets across entries are rejected.
- `build.generator` must be `cmake` or `script`; `script` requires
  `build.command`.

Invalid YAML fails dispatch: the push is recorded and
`dispatchError` surfaces in the webhook response (see
[Webhooks](#/docs/webhooks)), but no tasks are created.

## What the runner does

Each matched entry becomes a task graph (see
[Runner and tasks](#/docs/runner-strategy)); the stages run in order:

1. **clone**: the *server* clones the code repository at the pushed
   commit, packs the working tree into a tarball and extracts it on the
   environment into ~/.md-builder/tasks/<sha12>/code.
2. **build**: a generated script exports MD_COMMIT, MD_ENV_NAME,
   MD_ENV_TAGS, MD_CODE_DIR (`…/code`) plus the yaml env variables, then
   runs the build stage in the code directory.
3. If the build (or the clone) fails, the dependent test stages are
   marked skipped and the dashboard shows ✗.
4. **unit / regression**: the stage command runs in the code directory,
   each under its timeout; the full output streams into the task log and
   the outcome is stored as a test run.

## Results files

A stage command may produce structured results files — by default the
googletest XML (`--gtest_output=xml:`) or JSON (`--gtest_output=json:`)
format, which the test binary writes itself. A run can produce several of
them; `results` accepts a single path or a list:

```yaml
unit:
  command: "./build/unit_tests --gtest_output=xml:build/test_detail.xml"
  results: "build/test_detail.xml"
```

```yaml
unit:
  command: "ctest --output-junit junit.xml && ./build/extra_tests --gtest_output=json:build/extra.json"
  results: ["build/test_detail.xml", "build/extra.json"]
```

When `results` is set, the runner reads every configured file back after
the command and:

- stores each one **verbatim** as its own run artifact, and
- extracts only the aggregate counts (total / failed / skipped) from each
  file's root attributes, **summed across all files**, for the matrix cell.

The per-case list — name, status, duration, failure message — is parsed
**in the browser** when you open the run's detail page; the server never
interprets individual cases. Every stored results file is parsed by its
default name/format; files that are missing or unrecognized are skipped
(noted in the stage log) and do not fail the run — it still records the
command's exit status. Paths are relative to the code directory (absolute
paths work too).

The same field works for regression stages, and regression reports may
additionally submit per-case rows (status, error value, duration) through
the reporting API.

## Custom summaries

A stage command may print a summary line on stdout:

```
MD-BUILDER-SUMMARY: all 8 tests passed, max rel err 3.2e-7
```

The text after the prefix becomes the run summary shown on the dashboard.
Without it, the summary is the exit code plus the last lines of the stage
log (truncated to 500 characters).
