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
version: 3

# Optional defaults, merged into every matrix entry (maps merge key-wise,
# scalars are overridden per entry).
defaults:
  timeout: 3600                 # per-command timeout seconds (hard cap 4h);
                                # a command that outlives it is reported
                                # with the "timeout" status
  env:
    OMP_NUM_THREADS: "4"
  variables:                    # templates, expanded on the environment
    BUILD_ROOT: "$MD_CODE_DIR/build"
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
    variables:                    # merged over defaults.variables
      CMAKE_FLAGS: "-DENABLE_MPI=OFF"
    build:
      command: 'cmake -B "$BUILD_ROOT" $CMAKE_FLAGS . && cmake --build "$BUILD_ROOT"'
      description: "Build the code with gcc and cmake"
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      description: "Run unit tests"
      timeout: 600
      artifacts: "build/test_detail.xml"  # googletest XML file
    regression:
      description: "Run regression tests"   # label of the stage as a whole
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
| version                      | yes      | Must be 3.                                                          |
| defaults                     | no       | Entry-level defaults: timeout, env, variables, build, unit, regression. |
| presets                      | no       | Shared regression cases (see below).                               |
| matrix                       | yes      | One or more entries; each entry needs tags and at least one stage.  |
| matrix[].tags                | yes      | Tags selecting the environment (see [Test environments](#/docs/environments)). Must be unique per entry. |
| matrix[].description         | no       | Human-readable label.                                               |
| matrix[].timeout             | no       | Default stage timeout in seconds (default 3600, capped at 14400).   |
| matrix[].env                 | no       | Extra environment variables exported for all stages — literal values. |
| matrix[].variables           | no       | Extra variables for this entry's stages, merged over defaults.variables — template values (see [Variables](#variables)). |
| matrix[].build               | no       | Build stage (see below).                                            |
| matrix[].unit                | no       | Unit test stage: at least command; optional description, workdir, timeout, artifacts. |
| matrix[].regression          | no       | Regression selection: use and/or disable referencing presets; optional description of the stage as a whole. |
| unit.command                 | yes (per stage) | Shell command, or a list of commands (see Command lists).    |
| unit.description             | no       | Human-readable label of the stage, shown on its run detail page and stored with every triggered run. |
| unit.workdir                 | no       | Directory the command runs in (see Working directories).            |
| unit.artifacts               | no       | Artifact file path (or list) the runner fetches back (see Artifact files). |
| unit.timeout                 | no       | Stage timeout overriding defaults.                                  |
| regression.description       | no       | Human-readable label of the regression stage as a whole (the container node); each case carries its preset's own description. |

The build stage is one shell command — or a list of them, like the
test stages (no built-in cmake support — write the
cmake/make/ninja/script invocation yourself):

| Field          | Description                                                          |
|----------------|----------------------------------------------------------------------|
| build.command  | Shell command compiling the code (required — entry or defaults), or a list of commands (see Command lists). |
| build.description | Human-readable label of the build stage, shown on its run detail page and stored with every triggered run (falls back to defaults.build.description). |
| build.workdir  | Directory the command runs in (same semantics as unit/presets).      |
| build.artifacts | Optional file path (or list) the runner fetches back after the build and stores on the build run — downloadable from the run's detail page. Never parsed: the build verdict is its exit code alone. |

Example — out-of-source cmake via the workdir and the built-in
`$MD_CODE_DIR` variable:

```yaml
build:
  command: 'cmake "$MD_CODE_DIR" -DCMAKE_BUILD_TYPE=Release && cmake --build . -j16'
  workdir: "build"   # runs in <code>/build
```

Every stage command runs under the remote `timeout` command, bounded by
the stage's timeout (default 3600s, capped at 4h); the stage's own SSH
session gets that timeout plus 5 minutes of slack.

A stage whose command outlives its timeout is a **`timeout`**, not a plain
`failed` — the outcome is the same, but the cause is what the pages show:

- the dashboard cell and the run page read "⏱ timeout" in their own color;
- the run's error and summary name the timeout and the command
  (`timed out after 30m0s: make -j8`, plus the log tail);
- the log gets a `task timed out: timed out after 30m0s: make -j8` line at
  the point the command was killed (its own output stays above it);
- the stages behind it are **skipped** with "upstream task build timed out"
  as the reason, exactly as a failure would leave them.

The remote `timeout` wrapper reports exit 124 when it fires, which is how
the runner tells a timeout from a command that exited 124 on its own
(nothing does, in practice). A command that ignores SIGTERM and outlives
the session's slack as well is cut by the session instead — same status,
same wording.

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
| presets.<name>.description | no | Human-readable label of the case, shown on the case's own task page (and its runs); stored with every triggered run. |
| presets.<name>.workdir | no    | Directory the command runs in (see Working directories).     |
| presets.<name>.timeout | no     | Case timeout (falls back to defaults/matrix timeout).        |
| presets.<name>.artifacts | no    | Artifact files to collect (see Artifact files).              |

Each referenced preset becomes its **own task** in the graph
(“regression: heat”), nested under the stage's virtual container. It runs
after the build with its own timeout, its own log, its own attempt history
and its own run — which is what lets it be reported on, downloaded and
re-run on its own. The cases run independently — one failing case does not
stop the others — and the matrix cell is the container's rollup over all
cases of the entry.

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

- **exit 0 → passed**; any non-zero exit → failed. A timeout is the one
  non-zero exit with a status of its own — **`timeout`**, recorded by the
  runner rather than derived from the exit code (see the timeout paragraph
  under [Field reference](#/docs/test-matrix)) — while a failed `cd` into
  the workdir is a plain failure.
- An SSH-level failure (host unreachable, session dropped) fails the
  case the same way, with the transport error as the case's note.
- The preset's `artifacts` files **never flip the verdict** — they are
  stored as artifacts of the case's own run (per-case detail parsed
  in the browser). This differs from the unit stage, where artifact files
  reporting failed cases also fail the run.
- The case's own run counts as the **one** test the case is: `1/1 passed`
  when the command exits 0, `0/1 failed` otherwise. That count is the
  verdict, not a tally parsed out of the artifact files (the browser
  parses those for the per-case detail).
- An `MD-BUILDER-SUMMARY:` line only becomes the case's note; it cannot
  turn a non-zero exit into a pass.

The matrix cell is the **container's** state, which is the rollup of the
cases under it: any failed case fails the stage and the cell says which
ones ("3/4 cases passed; failed: heat"), all of them passed and the stage
passes, and while they are still being claimed it reads "2/4 cases passed;
2 queued". A container whose failures are all timeouts reads `timeout`
instead, and names them ("1/4 cases passed; timed out: poisson"); a mix of
both reads `failed` and names both causes. Cases run independently — one
failing case does not stop the others. When the clone or build ends
without passing — it failed, it timed out, or it was itself reported
`skipped` — every case is marked **skipped** with the upstream error as
its summary (and one log line of its own); if every case was skipped, the
stage itself reads `skipped`, not `passed`.

## Command lists

The `command` of the build stage, a unit stage or a regression preset
may be **one command or a list**:

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
| MD_SECRET_TOKEN | The site's secret token (Settings → Repository), when one is configured — see below. |

The yaml `env` variables are exported right after the MD_* variables,
so a command can override neither (they are exported earlier — see the
env setup script below for the override point).

```yaml
unit:
  command: "$MD_CODE_DIR/build/unit_tests --gtest_output=xml:$MD_CODE_DIR/build/test_detail.xml"
```

These references expand because the command itself runs inside the
stage script, on the environment, where `MD_*` is exported. A value in
`env`, by contrast, is a literal that is never expanded — see
[Variables](#variables) for the expanding alternative.

### Secret token (MD_SECRET_TOKEN)

Commands frequently need credentials — a private package mirror, an
artifact store, a licensed-software license server — that must not be
committed to the code repository. The site's **secret token**
(Settings → Repository → *Secret token for commands*) covers this: when
configured, it is exported to every stage script (build, unit,
regression cases — the same stages as the other MD_* variables) as
`MD_SECRET_TOKEN`:

```yaml
build:
  command: "cmake -DFETCH_TOKEN=\"$MD_SECRET_TOKEN\" . && cmake --build . -j8"
```

```yaml
presets:
  eos-table:
    command: "curl -sS -H \"Authorization: Bearer $MD_SECRET_TOKEN\" -o eos.tbl https://data.internal/eos.tbl && ./check_eos eos.tbl"
```

The token is write-only like the repository access token: the settings
form shows whether one is set, never the value. If a command echoes it
(`env`, `set -x`, `curl -v`), the runner replaces every occurrence with
`REDACTED` in the task log before storing it. When no token is
configured the variable is simply unset.

## Variables

`defaults` and each matrix entry may declare **`variables`**: named
values that the entry's stage commands use like any shell variable.
They are the *expanding* counterpart of `env`, and the difference
matters:

- an **`env`** value is a literal. `BUILD_ROOT: "$MD_CODE_DIR/build"`
  under `env` exports that text, single-quoted; nothing expands it, so a
  command reading `$BUILD_ROOT` gets a path with a `$` inside it.
- a **`variables`** value is a template, expanded by the stage script
  **on the environment**, when the stage runs.

Three kinds of reference are substituted there:

1. the built-in `MD_*` variables (`$MD_CODE_DIR`, `$MD_COMMIT`, …);
2. the entry's **other variables** — exported in dependency order, so
   one may be written in terms of another;
3. the **host** environment variables **that environment** allows
   (*Runner Envs* → the row's **Edit** form — `HOME`, `USER`, `LOGNAME`,
   `PATH`, `SHELL`, `TMPDIR` until its owner changes the list). Each host
   publishes its own names, so the same yaml can expand on one machine and
   stay literal on another. A name the entry's own `env` block exports
   counts too while it is on that list: `env` is exported before the
   variables.

Everything else stays literal: a typo like `$MD_CODEDIR` reaches the
command as the text `$MD_CODEDIR`, and the stage logs

```
warning: variables.<name>: $MD_CODEDIR is not a built-in variable, a variables entry or an allowed environment variable; kept literally
```

so a path never silently loses its `$`.

```yaml
defaults:
  variables:
    BUILD_ROOT: "$MD_CODE_DIR/build"

matrix:
  - tags: [cpu]
    variables:
      CMAKE_FLAGS: "-DCMAKE_BUILD_TYPE=Release -DENABLE_MPI=OFF"
      UNIT_XML: "$BUILD_ROOT/tests/unit.xml"   # BUILD_ROOT expands first
    build:
      command: 'cmake -B "$BUILD_ROOT" $CMAKE_FLAGS . && cmake --build "$BUILD_ROOT" -j8'
    unit:
      command: "./build/unit_tests --gtest_output=xml:$UNIT_XML"
```

An entry inherits `defaults.variables` key-wise and wins on conflicts,
exactly like `env`.

### Values are data, never code

A value is rendered as **one shell word**: expandable references become
double-quoted `${NAME}`, everything around them is single-quoted. A
value containing `$(…)`, backticks, quotes or a newline is therefore
text: `INJECT: "$(touch /tmp/x)"` exports that string, and a command
echoing `$INJECT` prints it instead of running it. The yaml cannot
splice shell syntax into a stage script through `variables`.

A literal `$` in a value is written `$$` (`PRICE: "5$$ per run"`); a
`$` that is not followed by a name (`50$`, `${ }`) is left alone.

### Order of the preamble exports

Every stage script builds its environment in this order:

1. the built-in `MD_*` variables;
2. the entry's `env` (literals, single-quoted);
3. the **environment setup script** (sourced — see below);
4. the entry's `variables` (templates, expanded here);
5. the `cd` into the working directory.

Because variables are expanded after the environment setup script, a
variable may build on what that script exports (module loads, host
paths pushed by `module load`, …) — and a variable of the same name
wins over the script's export. The `cd` comes last, so `workdir` may
reference a variable too.

### Allowing host variables

Which host environment variables a repository's yaml may read is the
**host owner's** decision, not the repository's: the list lives on the
environment itself — *Runner Envs* → the row's **Edit** form,
**Expandable host variables** — so two machines of the same site may
expose different names (see
[Test environments](#/docs/environments)). A new environment starts on a
minimal default list (`HOME, USER, PATH, …`). A name that is not on the
list stays literal, exactly like a typo; an owner may also clear the list
to allow none at all.

### Validation

Variables are checked when the yaml is read, so a broken entry fails
the dispatch with the offending name (a `dispatchError` on the commit
row) instead of shipping a script whose values depend on the export
order:

- the name must be a shell identifier (`[A-Za-z_][A-Za-z0-9_]*`);
- it may not start with `MD_` — reserved for the built-in variables,
  which always expand;
- it may not also appear in the entry's `env` — the name would have two
  values with different semantics, so keep it in one of the two;
- two variables may not reference each other in a cycle.

## Environment setup script

Each **environment** (configured on the site, not in the yaml) may
carry an env setup script — the script content is edited in the
environment settings form and stored on the server. On dispatch it is
written to the task dir as `md-builder-env-<hash>.sh` and **sourced by
every stage script** before the stage command (module loads, compiler
exports, virtualenv activation, …):

- **present**: every stage script runs `. md-builder-env-<hash>.sh`
  first; anything the script exports is visible to the build, unit and
  regression commands. It runs after the built-in `MD_*` and yaml `env`
  exports and before the yaml `variables`, so it can override the
  former, while a variable of the same name wins over it (see
  [Order of the preamble exports](#order-of-the-preamble-exports)).
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

### Build artifacts

The **build** stage accepts the same `artifacts` field, with different
semantics: the files are stored **verbatim** and never parsed — the build's
verdict is its command's exit code alone (a results-looking file fetched
from a build cannot flip the cell to failed).

```yaml
build:
  command: "cmake . && ninja"
  artifacts: ["build/.ninja_log", "build/compile_commands.json"]
```

Any file the build leaves behind works — logs, `compile_commands.json`,
size reports. As with the test stages, paths are relative to the build's
workdir and each file is capped at 8 MiB at fetch time.

### Plot artifacts (`*.plot.json` / `*.plotly.json`)

A regression case's artifacts may include **plot figures**: files whose
name ends in `.plot.json` or `.plotly.json` and whose content is a
[Plotly] figure document — a `data` array of traces plus an optional
`layout` object:

```yaml
presets:
  heat:
    command: "python3 run_heat.py --plot drift.plot.json"
    workdir: "regression/heat"
    artifacts: "drift.plot.json"
```

```json
{
  "data": [
    { "x": [1, 2, 3], "y": [2, 6, 3], "type": "scatter", "mode": "lines+markers" }
  ],
  "layout": { "width": 320, "height": 240, "title": "A Fancy Plot" }
}
```

When you open the run detail page, every plot artifact is fetched and
rendered as an interactive chart (zoom, hover, legend toggle — the
standard Plotly toolbar). The document is passed through almost verbatim:
every [Plotly trace type] (scatter, bar, heatmap, 3D surface, …) works,
and the file's `layout` wins over the defaults. A malformed file shows its
error inline and still downloads from the artifacts table.

**Size.** The height is the file's to choose: `layout.height` is drawn as
written (a bare root-level `height` is honoured too, for hand-rolled
exports), inside loose bounds of 120–2000 px so a hairline or a runaway
value is not taken literally. A document that names no height gets 480 px
— taller than Plotly's own 450, because a chart on this page spans a
thousand pixels and a shorter default reads as squashed. The width is
always the container's: a `layout.width` in the file is dropped rather
than allowed to overflow the page. The label above each chart states the
height it was drawn at and where it came from — `800 px (from the file)`,
`480 px (default)`, or `(capped from 4000)` — so a figure that looks
wrong says why without anyone having to read the JSON.

The **name is what decides**, case-insensitively: only those two suffixes
are charted, because the alternative — fetching every JSON artifact and
checking its shape — would download a run's whole results files and pull
in the 3.5 MB renderer just to find out they are not figures. A figure
stored under another name (`results/nvt-compare.plotly.json` is fine;
`results/figure.json` is not) simply appears in the artifacts table like
any other file.

[Plotly]: https://plotly.com/javascript/
[Plotly trace type]: https://plotly.com/javascript/chart-studio/

Plot files are ordinary artifacts otherwise: stored verbatim, capped at
8 MiB, downloaded with the zip bundle, and irrelevant to the case's
verdict (the exit code decides, as always). They also work on unit and
build runs — the detail page charts them wherever they appear.

### HTML artifacts (`*.html`)

An artifact whose name ends in `.html` (or `.htm`) is shown as the page it
is: framed in the run detail page, and one click from a tab of its own. A
test that writes a Plotly HTML export, a coverage report or a
self-contained results page therefore needs nothing beyond listing the
file:

```yaml
unit:
  command: "pytest --html=report/report.html"
  artifacts: ["report/report.html"]
```

The same naming rule as plots applies — only those two suffixes are
framed, and the run page says so (`HTML pages`). The rendered page is
served by `GET /api/test-artifacts/{id}/raw`, the artifact endpoint's
view-don't-save form: the same bytes as the download, with a type the
browser renders instead of saving, and for HTML a sandbox.

**Why the sandbox matters.** An artifact is a build product: its content
comes from the code under test, not from md-builder. Rendered from this
site's own origin, a page's scripts could call the API with your session
(the session cookie is HttpOnly, which keeps it from being *read* — not
from being *used*). The response therefore carries
`Content-Security-Policy: sandbox allow-scripts …` without
`allow-same-origin`, and the frame repeats that list: the page's scripts
still run (a Plotly export has to, that is what makes it a page), but from
an opaque origin that can read no cookie or storage, and whose requests
carry no credentials. A report that leans on `localStorage` for its UI
state will degrade in the preview; download it and open it locally if you
need the full thing. SVG is deliberately *not* previewed for the same
reason — it is script-capable too — and neither are archives or binaries:
those stay downloads.

HTML artifacts are ordinary artifacts otherwise: stored verbatim, capped
at 8 MiB, and downloaded with the zip bundle. Like plots, they work on
build, unit and regression runs alike, and they never affect a stage's
verdict.

### Downloading artifacts

Every stored artifact of a run (build files, unit/regression results and
plot files) is downloadable from the run's detail page: each file
individually, or the attempt's whole bundle as one zip
(`GET /api/test-runs/{id}/artifacts/zip`). A zip of a run that produced
nothing is a `404` rather than an empty archive.

The zip that gathers a **whole test** is the task one:
`GET /api/tasks/{id}/artifacts/zip` bundles the latest attempts of the
task and every node under it — the task's own files at the archive root,
each descendant's under a directory named after it (`regression-heat/…`,
built from the node key with `:` replaced by `-`). Asking for the
regression container therefore downloads every case's files in one
archive, while asking for a single case downloads just that case. Retired
nodes are excluded: their files belong to an earlier graph shape.

File contents live in the configured S3-compatible object store (see
[Object storage](#/docs/object-storage)); a read whose object is missing is
a `404`, a backend failure a `502`.

## Validation rules

- `version` must be 3; `matrix` must be non-empty.
- Each entry needs non-empty `tags` and at least one of `unit` /
  `regression` (or its presets expansion), with a `command`.
- Duplicate tag sets across entries are rejected.
- `build.command` is required (the entry's own or `defaults.build`).
- Unknown fields are rejected (e.g. the removed `build.generator` /
  `cmake_flags` / `threads`).
- Every preset needs a `command`; names in `use` / `disable` must
  reference defined presets.
- `variables` names must be shell identifiers, must not start with
  `MD_`, must not repeat an `env` name of the same entry and must not
  form a reference cycle (see [Variables](#variables)).

Invalid YAML fails dispatch: the push is recorded and
`dispatchError` surfaces in the webhook response (see
[Webhooks](#/docs/webhooks)), but no tasks are created. The same message
is stored on the commit row, so the dashboard shows it in the commit's
**graph** column (a warning triangle: hover or click for the text) —
which is also what an entry matching no enabled environment reports.

Because matching is site-wide, a column may be a machine someone else
registered: each environment column of the dashboard names its owner under
the environment name (see [Test environments](#/docs/environments)).

## What the runner does

Each matched entry becomes a task graph (see
[Runner and tasks](#/docs/runner-strategy)); the stages run in order:

1. **clone**: the *server* clones the code repository at the pushed
   commit, packs the working tree into a tarball and extracts it on the
   environment into ~/.md-builder/tasks/<sha12>/code. The env setup
   script is written into the task dir.
2. **build**: a generated script exports MD_COMMIT, MD_ENV_NAME,
   MD_ENV_TAGS, MD_TASK_DIR, MD_CODE_DIR plus the yaml env variables,
   sources the env setup script (if any), exports the expanded yaml
   variables and runs the build stage in its working directory.
3. If the build (or the clone) ends without passing — it failed, it timed
   out, or it was itself reported `skipped` — the dependent test stages are
   marked **skipped**, with the reason in their summary and one line in
   their log, and the dashboard says so.
4. **unit**: the stage command runs in its working directory under its
   timeout; the full output streams into the task log and the outcome
   is stored as the attempt's test run.
5. **regression: one task per selected preset** — each case command
   runs after the build (exporting MD_CASE), collects its own artifact
   files and records its own run; the matrix cell shows the container's
   rollup across cases.

## Custom summaries

A stage command may print a summary line on stdout:

```
MD-BUILDER-SUMMARY: all 8 tests passed, max rel err 3.2e-7
```

The text after the prefix becomes the run summary shown on the dashboard.
Without it, the summary is the exit code plus the last lines of the stage
log (truncated to 500 characters). For a regression case the summary
line becomes that case's run summary. A timed-out stage keeps the
timeout's wording instead: what a killed command printed on its way out is
the tail of the summary, not the summary itself.
