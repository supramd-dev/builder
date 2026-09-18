// Minimal API client. Cookies (session token) are sent automatically since
// /api is same-origin (via the Vite dev proxy or the Go server).
// Dispatch endpoints (manual, manual-yaml, webhook replays) carry the
// failure reason in `dispatchError` while still returning partial results
// (e.g. the recorded commit) — prefer `error`, fall back to `dispatchError`.
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const data = (await res.json().catch(() => null)) as
    | (T & { error?: string; dispatchError?: string })
    | null
  if (!res.ok) {
    throw new Error(data?.error || data?.dispatchError || `Request failed (${res.status})`)
  }
  return data as T
}

export interface Me {
  username: string
  email: string
}

export interface TestEnvironment {
  id: number
  name: string
  host: string
  username: string
  tags: string[]
  description: string
  envScript: string
  enabled: boolean
  createdAt: string
  updatedAt: string
}

export interface EnvironmentInput {
  name: string
  host: string
  username: string
  privateKey: string
  tags: string[]
  description: string
  envScript: string
  enabled?: boolean
}

export interface ConnectivityResult {
  success: boolean
  message: string
  banner?: string
  durationMilliSeconds?: number
}

export async function listEnvironments(): Promise<TestEnvironment[]> {
  const res = await api<{ environments: TestEnvironment[] }>('/api/environments')
  return res.environments
}

export async function createEnvironment(
  input: EnvironmentInput,
): Promise<TestEnvironment> {
  return api<TestEnvironment>('/api/environments', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

export async function updateEnvironment(
  id: number,
  input: EnvironmentInput,
): Promise<TestEnvironment> {
  return api<TestEnvironment>(`/api/environments/${id}`, {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

export async function deleteEnvironment(id: number): Promise<void> {
  await api<{ status: string }>(`/api/environments/${id}`, { method: 'DELETE' })
}

export async function setEnvironmentEnabled(
  id: number,
  enabled: boolean,
): Promise<TestEnvironment> {
  return api<TestEnvironment>(`/api/environments/${id}/enabled`, {
    method: 'PUT',
    body: JSON.stringify({ enabled }),
  })
}

export async function testEnvironment(id: number): Promise<ConnectivityResult> {
  return api<ConnectivityResult>(`/api/environments/${id}/test`, {
    method: 'POST',
  })
}

export interface ExecResult {
  success: boolean
  stdout: string
  stderr: string
  exitCode: number
  durationMilliSeconds: number
}

export async function execEnvironment(
  id: number,
  command: string,
): Promise<ExecResult> {
  return api<ExecResult>(`/api/environments/${id}/exec`, {
    method: 'POST',
    body: JSON.stringify({ command }),
  })
}

export type ScriptLanguage = 'bash' | 'python'

export async function execScript(
  id: number,
  language: ScriptLanguage,
  script: string,
): Promise<ExecResult> {
  return api<ExecResult>(`/api/environments/${id}/script`, {
    method: 'POST',
    body: JSON.stringify({ language, script }),
  })
}

export interface SiteConfig {
  codeRepo: string
  accessTokenSet: boolean
  // IANA timezone name every timestamp is displayed in; "" = the viewer's
  // browser-local zone.
  timezone: string
  secretTokenSet: boolean
  updatedAt: string
}

// SiteConfigUpdate is the PUT body: the tokens are write-only.
// An empty value keeps the stored one; the clear flags remove it.
export interface SiteConfigUpdate {
  codeRepo: string
  accessToken?: string
  timezone?: string
  secretToken?: string
  clearAccessToken?: boolean
  clearSecretToken?: boolean
}

export async function getSiteConfig(): Promise<SiteConfig> {
  return api<SiteConfig>('/api/site-config')
}

// Cached site config: the config is read-only for most pages (the settings
// page is the only writer), so callers that just need e.g. the code repo
// share one request per session. updateSiteConfig invalidates the cache.
let siteConfigCache: SiteConfig | null = null
export async function cachedSiteConfig(): Promise<SiteConfig | null> {
  if (siteConfigCache) return siteConfigCache
  try {
    siteConfigCache = await getSiteConfig()
    return siteConfigCache
  } catch {
    return null
  }
}

export async function updateSiteConfig(
  input: SiteConfigUpdate,
): Promise<SiteConfig> {
  const cfg = await api<SiteConfig>('/api/site-config', {
    method: 'PUT',
    body: JSON.stringify(input),
  })
  siteConfigCache = cfg
  return cfg
}

// --- Test dashboard ---

export type DashboardKind = 'regression' | 'unit' | 'build' | 'full'

export interface DashboardEnvironment {
  id: number
  name: string
  description: string
  tags: string
  enabled: boolean
}

export interface DashboardCommit {
  id: number
  sha: string
  shortSha: string
  repo: string
  repoUrl?: string
  ref: string
  author: string
  message: string
  // what created the row: push | tag_push | merge_request | manual | manual_yaml
  // (absent on rows recorded before the column existed).
  event?: string
  pushedAt: string
  // true when a newer attempt of the same SHA exists (manual re-dispatch):
  // the row is kept for history but rendered dimmed.
  superseded?: boolean
}

export interface RunCell {
  runId: number
  taskId?: number
  // "skipped" marks the runner's recordSkippedRuns artifact: the stage
  // never ran because an upstream task failed (stored status is failed,
  // summary starts with "skipped:").
  status: 'passed' | 'failed' | 'running' | 'pending' | 'skipped'
  total: number
  passed: number
  failed: number
  trigger?: number // the root graph's trigger: 1 = manual, 2 = manual yaml, 0/absent = webhook
  startedAt: string
  finishedAt: string
  error?: string
}

// --- Jobs ---

export interface Job {
  id: number
  commitId: number
  environmentId: number
  tags: string
  status: 'pending' | 'running' | 'done' | 'failed'
  error: string
  attempts: number
  testInputRef: string
  startedAt: string
  finishedAt: string
}

export async function listJobs(limit = 20): Promise<Job[]> {
  const res = await api<{ jobs: Job[] }>(`/api/jobs?limit=${limit}`)
  return res.jobs
}

export async function triggerJobs(commitId: number): Promise<{
  jobsCreated: number
  entriesSkipped: number
}> {
  return api<{ jobsCreated: number; entriesSkipped: number }>('/api/jobs', {
    method: 'POST',
    body: JSON.stringify({ commitId }),
  })
}

// ManualTestInput is the POST /api/jobs/manual body: one repository (empty
// = the site-config default), an optional ref (empty = HEAD), the stage
// commands (an empty stage is skipped) and the environments to run on. The
// artifacts fields accept one path or a list — a run can produce several
// artifact files.
export interface ManualTestInput {
  repo?: string
  ref?: string
  buildCommand?: string
  unitCommand?: string
  unitArtifacts?: string | string[]
  regressionCommand?: string
  regressionArtifacts?: string | string[]
  environmentIds: number[]
}

export interface ManualTestRoot {
  taskId: number
  environmentId: number
}

export async function triggerManualTest(
  input: ManualTestInput,
): Promise<{ roots: ManualTestRoot[] }> {
  return api<{ roots: ManualTestRoot[] }>('/api/jobs/manual', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// ManualYAMLResult is the POST /api/jobs/manual-yaml response: the resolved
// commit (0 when resolution failed) and how many graphs were created.
export interface ManualYAMLResult {
  commitId: number
  commitSha: string
  commitCreated: boolean
  jobsCreated: number
  entriesSkipped: number
  dispatchError?: string
}

// triggerManualYAML dispatches the md-builder.yaml matrix of the given ref
// (branch / tag / commit id; empty = HEAD) of the site-configured code
// repository — the webhook flow, started by hand.
export async function triggerManualYAML(ref: string): Promise<ManualYAMLResult> {
  return api<ManualYAMLResult>('/api/jobs/manual-yaml', {
    method: 'POST',
    body: JSON.stringify({ ref }),
  })
}

export interface DashboardRow {
  commit: DashboardCommit
  cells: (RunCell | null)[]
}

export interface Dashboard {
  kind: DashboardKind
  repoFilter?: string
  // Web URL of the filtered repository, when derivable — the header repo
  // links there.
  repoUrl?: string
  environments: DashboardEnvironment[]
  rows: DashboardRow[] // one row per commit, newest first
}

export async function getDashboard(
  kind: DashboardKind,
  commits = 10,
): Promise<Dashboard> {
  // The full kind has its own wire shape (see getFullDashboard).
  return api<Dashboard>(`/api/dashboard/${kind}?commits=${commits}`)
}

// --- Full dashboard (all stages per commit × environment) ---

// FullStage is one stage cell of the full matrix: a recorded run
// (build/unit/regression) or a live task state (runId 0).
export interface FullStage {
  kind: 'build' | 'unit' | 'regression'
  runId: number
  taskId?: number
  status: string
  error?: string
  summary?: string
  startedAt: string
  finishedAt: string
}

export interface FullRow {
  commit: DashboardCommit
  // environment id → stages in display order (build, unit, regression)
  stages: Record<string, FullStage[]>
  // environment id → root task id (the dependency graph link)
  taskIds: Record<string, number>
  // environment id → root trigger (1 = manual, absent = webhook)
  triggers?: Record<string, number>
}

export interface FullDashboard {
  repoFilter?: string
  repoUrl?: string
  environments: DashboardEnvironment[]
  rows: FullRow[]
}

export async function getFullDashboard(
  commits = 10,
): Promise<FullDashboard> {
  return api<FullDashboard>(`/api/dashboard/full?commits=${commits}`)
}

// CaseResult is one case in a regression run's case list: a summary of the
// case's own child TestRun — id doubles as the runId the UI navigates to.
export interface CaseResult {
  id: number
  name: string
  // "skipped" marks a case whose sub-task never ran (upstream failure);
  // "pending"/"running" are dispatch-time placeholders — the case's stage
  // sub-task has not reported yet.
  status: 'passed' | 'failed' | 'skipped' | 'pending' | 'running'
  message: string
  durationMillis: number
}

// TestArtifactRef references one stored result/log/series/file artifact of a
// run; the content itself is fetched via getTestArtifact (or the download
// endpoint, which serves the same bytes as a file).
export interface TestArtifactRef {
  id: number
  kind: 'results' | 'log' | 'series' | 'file'
  name: string
  size: number
}

export interface TestArtifactContent {
  id: number
  runId: number
  kind: string
  name: string
  content: string
}

export interface TestRunDetail {
  id: number
  kind: DashboardKind
  // "pending"/"running" are dispatch-time placeholders: the run follows its
  // stage sub-task live until the real outcome lands.
  status: 'passed' | 'failed' | 'skipped' | 'pending' | 'running'
  summary: string
  // Child runs carry the case (preset) name and message; empty on
  // top-level runs.
  name: string
  message: string
  total: number
  passed: number
  failed: number
  skipped: number
  // Stage sub-task that produced the run (0 = external report); its log
  // (stdout) is shown on the detail page.
  taskId: number
  // Root of the producing stage task (0 = external report) — the breadcrumb
  // link to the graph page.
  rootTaskId: number
  // Parent regression run (0 = top-level run); a child's detail page links
  // back up, parentName is the parent's preset name (usually null).
  parentRunId: number
  parentName: string | null
  environmentId: number
  environmentName: string | null
  commitId: number
  commitSha: string | null
  commitShortSha: string | null
  commitMessage: string | null
  commitAuthor: string | null
  // Repository location and web URL (when derivable) — the run detail page
  // links to the commit on the hosting site.
  commitRepo: string | null
  commitRepoUrl: string | null
  startedAt: string
  finishedAt: string
  cases: CaseResult[]
  artifacts: TestArtifactRef[]
}

export async function getTestRun(id: number): Promise<TestRunDetail> {
  return api<TestRunDetail>(`/api/test-runs/${id}`)
}

// getTestArtifact fetches one stored artifact's raw content (the browser
// parses googletest results files client-side).
export async function getTestArtifact(id: number): Promise<TestArtifactContent> {
  return api<TestArtifactContent>(`/api/test-artifacts/${id}`)
}

// --- Tasks (runner component) ---

export type TaskStatus = 'pending' | 'running' | 'done' | 'failed' | 'skipped'

export type TaskKind = 'root' | 'clone' | 'build' | 'unit' | 'regression'

// SubTask is one node of a task graph (clone/build/unit/...). Test stages
// carry runId: the recorded test run for the stage-detail link.
export interface SubTask {
  id: number
  kind: TaskKind
  name: string
  status: TaskStatus
  error: string
  dependsOn: number[]
  runId?: number
  startedAt: string
  finishedAt: string
}

export interface TaskCommit {
  id: number
  sha: string
  shortSha: string
  repo: string
  repoUrl?: string
  ref: string
  author: string
  message: string
  pushedAt: string
}

export interface TaskEnvironment {
  id: number
  name: string
  description: string
  tags: string
  enabled: boolean
}

// TaskDetail is GET /api/tasks/{id}: the task plus, for a root, its
// sub-tasks and commit/environment context.
export interface TaskDetail {
  id: number
  rootId: number
  kind: TaskKind
  name: string
  status: TaskStatus
  error: string
  attempts: number
  commitId: number
  environmentId: number
  tags: string
  trigger?: number // 1 = manual, 2 = manual yaml, 0/absent = webhook
  startedAt: string
  finishedAt: string
  subTasks?: SubTask[]
  commit?: TaskCommit | null
  environment?: TaskEnvironment | null
}

export async function getTask(id: number): Promise<TaskDetail> {
  return api<TaskDetail>(`/api/tasks/${id}`)
}

// LogChunk is one stored chunk of a task's incremental log.
export interface LogChunk {
  seq: number
  content: string
}

export interface TaskLogs {
  chunks: LogChunk[]
  lastSeq: number
}

// getTaskLogs returns log chunks after the given sequence (0 = from the
// beginning) — poll with the lastSeq to follow a running task.
export async function getTaskLogs(
  id: number,
  after = 0,
): Promise<TaskLogs> {
  return api<TaskLogs>(`/api/tasks/${id}/log?after=${after}`)
}

// ServerHealth is the unauthenticated health probe; version is the served
// build's source revision (git commit id, "dev" for an unstamped build).
export interface ServerHealth {
  status: string
  version?: string
}

// getHealth fetches the server health/version (no session needed).
export async function getHealth(): Promise<ServerHealth> {
  return api<ServerHealth>('/api/health')
}

// --- Site health board ---

// HealthCheck is one probe of the deep health endpoint: the git repository
// reachability, the future object storage, ...
export interface HealthCheck {
  name: string
  status: 'ok' | 'fail' | 'skipped'
  target?: string
  detail?: string
  durationMillis: number
}

// DeepHealth is GET /api/health/deep: the liveness probe plus one row per
// external dependency, probed on demand.
export interface DeepHealth {
  version?: string
  checks: HealthCheck[]
  checkedAt: string
}

// getDeepHealth runs the site's dependency probes (git repo, object
// storage) and returns their outcomes. Requires a session.
export async function getDeepHealth(): Promise<DeepHealth> {
  return api<DeepHealth>('/api/health/deep')
}

