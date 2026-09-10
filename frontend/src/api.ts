// Minimal API client. Cookies (session token) are sent automatically since
// /api is same-origin (via the Vite dev proxy or the Go server).
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const data = (await res.json().catch(() => null)) as (T & { error?: string }) | null
  if (!res.ok) {
    throw new Error(data?.error || `Request failed (${res.status})`)
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

export interface BuildTestResult {
  success: boolean
  stdout: string
  stderr: string
  exitCode: number
  durationMilliSeconds: number
}

// buildTest clones the site-configured code repository on the server,
// uploads it to the remote environment and runs a build command there.
export async function buildTest(
  id: number,
  buildCommand: string,
  ref: string,
): Promise<BuildTestResult> {
  return api<BuildTestResult>(`/api/environments/${id}/build-test`, {
    method: 'POST',
    body: JSON.stringify({ buildCommand, ref }),
  })
}

export interface SiteConfig {
  codeRepo: string
  testInputRepo: string
  testRepoRef: string
  deployKeySet: boolean
  deployTokenSet: boolean
  deployTokenUser: string
  updatedAt: string
}

// SiteConfigUpdate is the PUT body: the secret fields are write-only.
// An empty deployKey/deployToken keeps the stored one; the clear flags
// remove it.
export interface SiteConfigUpdate {
  codeRepo: string
  testInputRepo: string
  testRepoRef: string
  deployKey?: string
  deployToken?: string
  deployTokenUser?: string
  clearDeployKey?: boolean
  clearDeployToken?: boolean
}

export async function getSiteConfig(): Promise<SiteConfig> {
  return api<SiteConfig>('/api/site-config')
}

export async function updateSiteConfig(
  input: SiteConfigUpdate,
): Promise<SiteConfig> {
  return api<SiteConfig>('/api/site-config', {
    method: 'PUT',
    body: JSON.stringify(input),
  })
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
  ref: string
  author: string
  message: string
  pushedAt: string
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

export interface DashboardRow {
  commit: DashboardCommit
  cells: (RunCell | null)[]
}

export interface Dashboard {
  kind: DashboardKind
  repoFilter?: string
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
}

export interface FullDashboard {
  repoFilter?: string
  environments: DashboardEnvironment[]
  rows: FullRow[]
}

export async function getFullDashboard(
  commits = 10,
): Promise<FullDashboard> {
  return api<FullDashboard>(`/api/dashboard/full?commits=${commits}`)
}

export interface CaseResult {
  id: number
  name: string
  status: 'passed' | 'failed'
  errorValue: number
  message: string
}

export interface TestRunDetail {
  id: number
  kind: DashboardKind
  // "skipped" marks the runner's recordSkippedRuns artifact: the stage
  // never ran because an upstream task failed (stored status is failed,
  // summary starts with "skipped:").
  status: 'passed' | 'failed' | 'skipped'
  summary: string
  total: number
  passed: number
  failed: number
  environmentId: number
  environmentName: string | null
  commitId: number
  commitSha: string | null
  commitShortSha: string | null
  commitMessage: string | null
  commitAuthor: string | null
  startedAt: string
  finishedAt: string
  cases: CaseResult[]
}

export async function getTestRun(id: number): Promise<TestRunDetail> {
  return api<TestRunDetail>(`/api/test-runs/${id}`)
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
