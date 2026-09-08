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
