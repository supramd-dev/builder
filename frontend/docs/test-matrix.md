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
| matrix[].unit                | no       | Unit test stage: at least command; optional timeout.                |
| matrix[].regression          | no       | Regression test stage: at least command; optional timeout.          |
| unit.command / regression.command | yes (per stage) | Shell command run in the code directory.                    |

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
[Webhooks](#/docs/webhooks)), but no jobs are created.

## What the runner does

For each job the generated script, on the environment:

1. Clones the **test input repository** (at the configured ref) and the
   **code repository**, checking out the pushed commit, into
   ~/.md-builder/jobs/<sha>.
2. Exports MD_COMMIT, MD_ENV_NAME, MD_ENV_TAGS, MD_CODE_DIR,
   MD_TEST_INPUT_DIR plus the yaml env variables.
3. Runs the build stage. If it fails, the test stages are reported as
   failed (skipped) with the build log tail.
4. Runs the unit / regression stages, each under its timeout.
5. Prints a line-protocol report that the server parses and stores.

## Custom summaries

A stage command may print a summary line on stdout:

```
MD-BUILDER-SUMMARY: all 8 tests passed, max rel err 3.2e-7
```

The text after the prefix becomes the run summary shown on the dashboard.
Without it, the summary is the exit code plus the last lines of the stage
log (truncated to 500 characters).
