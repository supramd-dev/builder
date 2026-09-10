# Overview

md-builder is a test platform for scientific computing software: it runs
your build and test commands on a pool of remote environments for every
push, and shows the results in a pass/fail matrix (one row per commit, one
column per environment).

The workflow in short:

1. **Configure the site** (Settings): the code repository, the test input
   repository and the ref to test against — plus optional credentials for
   private repositories.
2. **Register environments** (User center): the remote hosts that will run
   the tests, each labeled with tags.
3. **Add a test matrix** (md-builder.yaml at the root of the code
   repository): which tags run what commands.
4. **Push**: a GitLab webhook dispatches a task graph (clone → build →
   tests) per matrix entry to a matching environment; the results appear
   on the dashboard and each stage's log can be followed live.
