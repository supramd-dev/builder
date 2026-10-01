// ApiError is a failed request. It keeps the parsed response body: a dispatch
// that fails part way down its work still reports what it managed to create
// (e.g. the graphs already queued), and the caller can only show that by
// reading the body of the error.
export class ApiError extends Error {
  readonly status: number
  readonly body: unknown

  constructor(message: string, status: number, body: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }
}

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
    throw new ApiError(
      data?.error || data?.dispatchError || `Request failed (${res.status})`,
      res.status,
      data,
    )
  }
  return data as T
}

// Role is read-only everywhere in the UI: an administrator account is created
// with `md-builder adduser -admin`, and no request can change a role.
export type Role = 'admin' | 'user'

export interface Me {
  id: number
  username: string
  email: string
  role: Role
}

// SetupInput is the POST body of the first-run setup page: the code
// repository, and the account that becomes the site's first administrator.
// The access token is optional (a public repository needs none), and the
// webhook needs nothing here — the site generates its own secret.
export interface SetupInput {
  codeRepo: string
  accessToken: string
  username: string
  email: string
  password: string
}

// getSetupState asks whether the site still needs its first-run setup. It is
// unauthenticated (it runs before any account exists), so it answers a single
// boolean: true while the database has no account at all.
export async function getSetupState(): Promise<boolean> {
  const res = await api<{ required: boolean }>('/api/setup')
  return res.required
}

// completeSetup creates the first administrator and stores the code
// repository, then signs the new account in — the caller receives the same
// `Me` a login returns. It fails with 409 once the site has an account.
export async function completeSetup(input: SetupInput): Promise<Me> {
  return api<Me>('/api/setup', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// GitLabStartURL is where the browser goes to begin a GitLab sign-in. It is
// a full-page navigation, not a fetch: the server answers with a redirect to
// the GitLab authorization page, which must be followed by the browser so the
// address bar shows GitLab while the user consents.
export const gitlabStartURL = '/api/auth/gitlab/start'

// getGitLabEnabled asks whether the site offers GitLab sign-in. It is
// unauthenticated, so the login page can call it before anyone has signed in.
export async function getGitLabEnabled(): Promise<boolean> {
  try {
    const res = await api<{ enabled: boolean }>('/api/auth/gitlab/enabled')
    return res.enabled
  } catch {
    // A site that cannot answer (older server, network hiccup) simply does
    // not offer the button.
    return false
  }
}

// AccountSource is where an account came from: a local registration (created
// by an administrator or the first-run setup) or a GitLab sign-in. Read-only.
export type AccountSource = 'local' | 'gitlab'

// Account is one row of the administrator's account list. The password hash
// never leaves the server.
export interface Account {
  id: number
  username: string
  email: string
  role: Role
  disabled: boolean
  createdAt: string
  source: AccountSource
  // approved is false while the account waits for an administrator to admit
  // it. Every GitLab self-registration starts unapproved and cannot sign in
  // until it is admitted.
  approved: boolean
  // gitlabId is the account's user id on the GitLab instance, 0 for a local
  // account.
  gitlabId: number
}

// AccountUpdate is the PUT body. An empty password keeps the stored one; an
// omitted disabled or approved keeps the current state. `role` and `source`
// are deliberately absent — neither is a setting.
export interface AccountUpdate {
  username: string
  email: string
  password?: string
  disabled?: boolean
  approved?: boolean
}

export async function listAccounts(): Promise<Account[]> {
  const res = await api<{ users: Account[] }>('/api/users')
  return res.users
}

export async function updateAccount(
  id: number,
  input: AccountUpdate,
): Promise<Account> {
  return api<Account>(`/api/users/${id}`, {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

export interface TestEnvironment {
  id: number
  // The account that manages this environment. Every signed-in user sees the
  // whole site-wide pool, but only the owner and the administrators may
  // change a row or use its private key.
  owner: string
  canEdit: boolean
  name: string
  host: string
  username: string
  tags: string[]
  description: string
  envScript: string
  // The host environment variables a md-builder.yaml `variables:` value may
  // expand on THIS machine, as the runner will use them (the owner's list, or
  // the default one while it is unset). Names only — never values — so it is
  // reported with the rest of the row, which everybody can read. The list is
  // per environment because the yaml is repository-side: which of a host's
  // variables it may read is the decision of whoever runs that host, and two
  // hosts of the same site may well differ. allowedEnvVarsDefault is the
  // built-in minimal list the form offers back.
  allowedEnvVars: string[]
  allowedEnvVarsDefault: string[]
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
  // Names separated by commas, spaces or newlines. Optional on purpose: the
  // server treats an absent field as "leave it alone", so a form save that
  // does not touch this box never rewrites the list. On create there is
  // nothing to leave alone and the built-in default list is stored; an empty
  // string is a real value (nothing expands here) and must be sent as such.
  allowedEnvVars?: string
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
  // The webhook shared secret GitLab must send back in X-Gitlab-Token.
  // Unlike the two tokens above it is readable — it has to be copied into
  // GitLab by hand — but only for an administrator: the API leaves it empty
  // for everybody else.
  webhookToken: string
  updatedAt: string

  // The GitLab sign-in integration. Whether it is on and whether a client
  // secret is stored are plain booleans, so everyone sees them (the login
  // page needs the first one); the instance address and the application id
  // come back for administrators only.
  gitlabLoginEnabled: boolean
  gitlabClientSecretSet: boolean
  gitlabUrl: string
  gitlabClientId: string
  // The callback URL to register on the GitLab application, built by the
  // server from server.publicURL. Empty when the site address is not
  // configured, in which case GitLab sign-in cannot work at all.
  gitlabRedirectUri: string

}

// SiteConfigUpdate is the PUT body: the tokens are write-only.
// An empty value keeps the stored one; the clear flags remove it.
//
// The gitlab* fields are administrators only — the API refuses the request
// otherwise. gitlabLoginEnabled is a boolean rather than optional-nullable
// because the server treats an absent field as "leave it alone", so simply
// omitting it is what keeps the current state.
export interface SiteConfigUpdate {
  codeRepo: string
  accessToken?: string
  timezone?: string
  secretToken?: string
  clearAccessToken?: boolean
  clearSecretToken?: boolean
  gitlabUrl?: string
  gitlabClientId?: string
  gitlabClientSecret?: string
  clearGitlabClientSecret?: boolean
  gitlabLoginEnabled?: boolean
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

// rotateWebhookToken replaces the webhook shared secret (administrators
// only) and returns the updated configuration. It is its own call so the
// rotation cannot carry — and so cannot overwrite — the other settings.
export async function rotateWebhookToken(): Promise<SiteConfig> {
  const cfg = await api<SiteConfig>('/api/site-config/webhook-token', {
    method: 'POST',
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
  // The account that manages this environment. The matrix is site-wide — a
  // column may be someone else's machine, since dispatch matches yaml entries
  // against every enabled environment — so the header names the owner.
  owner?: string
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
  // Why this commit produced no task graph — the webhook's dispatchError,
  // recorded at dispatch time (the yaml could not be read or parsed, no entry
  // matched an environment, no code repo configured). Absent when a graph was
  // created; the matrix shows it on the cells that have no graph.
  dispatchError?: string
}

// RunCell is one stage cell of the matrix: the state of that stage's task
// node. runId is 0 when the node has no run of its own — the regression
// node is a virtual container, so its counts are its cases' and its detail
// is the task, not a run. taskId is always set, so the cell is clickable
// either way.
export interface RunCell {
  runId: number
  taskId?: number
  // "skipped" is a status of its own: the stage never ran because an
  // upstream task failed, and its summary carries the reason.
  status: 'passed' | 'failed' | 'running' | 'pending' | 'skipped'
  total: number
  passed: number
  failed: number
  skipped: number
  // The node's summary line: the failure text when it failed, the skip
  // reason when it was skipped.
  summary?: string
  trigger?: number // the root graph's trigger: 1 = manual, 2 = manual yaml, 0/absent = webhook
  startedAt: string
  finishedAt: string
  error?: string
}

// --- Dispatch ---

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

// triggerManualTest queues a manual test. The environments are dispatched one
// after another, so a failure part way down the list is a 422 that still
// reports the graphs created before it — the roots are in the ApiError body
// (see api()), not in a return value.
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

// Run is one stored attempt of one task — the unit of test history. A task
// gets one run per dispatch and one more per retry, so a task's runs are its
// attempts, newest first.
export interface Run {
  id: number
  taskId: number
  attempt: number
  kind: TaskKind
  // "pending"/"running" are live states: the attempt is open until the stage
  // reports its outcome.
  status: 'passed' | 'failed' | 'skipped' | 'pending' | 'running'
  // The task's summary line: the failure text, the skip reason, or what the
  // stage printed for MD-BUILDER-SUMMARY.
  summary: string
  total: number
  passed: number
  failed: number
  skipped: number
  durationMillis: number
  environmentId: number
  commitId: number
  startedAt: string
  finishedAt: string
}

// TestRunDetail is GET /api/test-runs/{id}: one attempt of one task, the
// task's identity (the run itself only knows its task id) and every other
// attempt of that same task. The pointer fields are null when the referenced
// record was deleted.
export interface TestRunDetail extends Run {
  taskName: string | null
  // Human label of the task from md-builder.yaml, when it set one.
  taskDescription: string | null
  // The node key: stable identity of the task inside its graph, so a retry
  // after a yaml change still points at the same node.
  taskKey: string | null
  taskKind: TaskKind | null
  // Root of the task's graph (0 = the task is its own root) — the graph-page
  // link.
  rootTaskId: number
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
  // Every attempt of this task, newest first. The current one is the run
  // this page is about; the rest are its retry history.
  attempts: Run[]
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

// testArtifactDownloadUrl is one artifact's content as a file download (the
// same bytes getTestArtifact returns). A plain link: the session cookie
// authenticates it.
export function testArtifactDownloadUrl(id: number): string {
  return `/api/test-artifacts/${id}/download`
}

// runArtifactsZipUrl is one attempt's artifacts as a zip. The endpoint 404s
// when the attempt produced none.
export function runArtifactsZipUrl(runId: number): string {
  return `/api/test-runs/${runId}/artifacts/zip`
}

// --- Tasks (runner component) ---

// TaskStatus is one vocabulary for tasks and runs: a task's status is its
// latest attempt's (a virtual node's is rolled up from its children).
export type TaskStatus = 'pending' | 'running' | 'passed' | 'failed' | 'skipped'

// isTerminalStatus reports whether a status is final, i.e. can no longer
// change: everything else (the two in-flight ones, and anything this build
// does not know — an older row's status, say) is treated as still moving. It
// mirrors the server's store.TaskStatusTerminal.
//
// Callers ask this question the other way round on purpose: a page that
// follows a task decides to *keep* following, and a whitelist of the live
// statuses would stop dead — with no request left to notice the change — on
// a status it does not recognise.
export function isTerminalStatus(status: string): boolean {
  return status === 'passed' || status === 'failed' || status === 'skipped'
}

// TaskKind is a node's role in the graph: the virtual root per (commit,
// environment), the three real stages, the virtual regression container and
// the real case tasks under it.
export type TaskKind =
  | 'root'
  | 'clone'
  | 'build'
  | 'unit'
  | 'regression'
  | 'regression_case'

// TaskNode is one node of a task graph. A real node (virtual false) is one
// that has runs of its own; runId is its latest attempt's run, for the
// detail link. A virtual node's counts are its children's.
export interface TaskNode {
  id: number
  // parentId nests the node in the tree (0 = directly under the root); the
  // graph view draws the edges of dependsOn instead — the scheduling order.
  parentId?: number
  kind: TaskKind
  // nodeKey is the node's identity inside its graph, stable across
  // dispatches: "build", "unit", "regression:<case>".
  nodeKey: string
  name: string
  description?: string
  virtual?: boolean
  // retired marks a node a later dispatch dropped from the yaml: it keeps
  // its runs, logs and artifacts as history, and is never scheduled again.
  retired?: boolean
  status: TaskStatus
  summary?: string
  error?: string
  dependsOn: number[]
  total: number
  passed: number
  failed: number
  skipped: number
  // attempts counts the dispatches of a real task (its runs).
  attempts: number
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

// TaskDetail is GET /api/tasks/{id}: the task itself (root or node), its
// runs, and — for a root — the graph's nodes: the current ones and the
// retired ones a later dispatch dropped.
export interface TaskDetail {
  id: number
  rootId: number
  parentId?: number
  kind: TaskKind
  nodeKey: string
  name: string
  description?: string
  virtual?: boolean
  retired?: boolean
  status: TaskStatus
  summary?: string
  error?: string
  total: number
  passed: number
  failed: number
  skipped: number
  attempts: number
  commitId: number
  environmentId: number
  tags: string
  trigger?: number // 1 = manual, 2 = manual yaml, 0/absent = webhook
  startedAt: string
  finishedAt: string
  // runs: a real task's attempts, newest first.
  runs?: Run[]
  // subTasks: a root's current nodes (the graph). retiredTasks: the history.
  subTasks?: TaskNode[]
  retiredTasks?: TaskNode[]
  commit?: TaskCommit | null
  environment?: TaskEnvironment | null
}

export async function getTask(id: number): Promise<TaskDetail> {
  return api<TaskDetail>(`/api/tasks/${id}`)
}

// getTaskRuns lists a real task's attempts, newest first.
export async function getTaskRuns(id: number): Promise<Run[]> {
  const res = await api<{ runs: Run[] }>(`/api/tasks/${id}/runs`)
  return res.runs ?? []
}

// LogChunk is one stored chunk of a task's incremental log.
export interface LogChunk {
  seq: number
  content: string
}

export interface TaskLogs {
  // attempt is the attempt the chunks come from: the task's current one
  // unless the caller asked for another (a retry's log stays readable).
  attempt: number
  chunks: LogChunk[]
  lastSeq: number
}

// getTaskLogs returns log chunks after the given sequence (0 = from the
// beginning) — poll with the lastSeq to follow a running task. attempt
// selects one of the task's attempts; omit it for the current one.
export async function getTaskLogs(
  id: number,
  after = 0,
  attempt?: number,
): Promise<TaskLogs> {
  const q = new URLSearchParams({ after: String(after) })
  if (attempt !== undefined) q.set('attempt', String(attempt))
  return api<TaskLogs>(`/api/tasks/${id}/log?${q}`)
}

// taskLogDownloadUrl is one attempt's whole log as a downloadable text file.
// The viewer follows the stream incrementally and keeps only its tail, so the
// file is assembled server-side and fetched by the browser itself (a plain
// link — the session cookie authenticates it). The filename gains an
// -attempt-N suffix when the caller asks for an attempt other than the
// current one.
export function taskLogDownloadUrl(id: number, attempt?: number): string {
  const q = attempt === undefined ? '' : `?attempt=${attempt}`
  return `/api/tasks/${id}/log/download${q}`
}

// taskArtifactsZipUrl is a task's artifacts — its own and every node's below
// it, each under a directory named from that node — as a zip. The endpoint
// 404s when the subtree stored none.
export function taskArtifactsZipUrl(id: number): string {
  return `/api/tasks/${id}/artifacts/zip`
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

