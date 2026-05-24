import type { ActivityItem, Project, Session, Task } from './types'

const API_BASE = '/api/v1'

async function request<T>(path: string): Promise<T> {
	const response = await fetch(`${API_BASE}${path}`, { headers: { Accept: 'application/json' } })
	if (!response.ok) throw new Error(`request failed: ${response.status}`)
	return response.json() as Promise<T>
}

async function requestJSON<T>(path: string, init: RequestInit): Promise<T> {
	const response = await fetch(`${API_BASE}${path}`, {
		headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
		...init,
	})
	if (!response.ok) throw new Error(`request failed: ${response.status}`)
	return response.json() as Promise<T>
}

async function requestForm<T>(path: string, formData: FormData): Promise<T> {
	const response = await fetch(`${API_BASE}${path}`, {
		method: 'POST',
		headers: { Accept: 'application/json' },
		body: formData,
	})
	if (!response.ok) throw new Error(`request failed: ${response.status}`)
	return response.json() as Promise<T>
}

export async function loadProjects(): Promise<Project[]> {
	const response = await request<{ items: Array<{ id: string; slug: string; title: string; description: string; open: number; in_progress: number; in_review: number; done: number; cancelled: number; updated_at: string }> }>('/projects')
	const items = response.items
	return items.map((item) => ({
		id: item.id,
		name: item.title,
		slug: item.slug,
		open: item.open ?? 0,
		inProgress: item.in_progress ?? 0,
		inReview: item.in_review ?? 0,
		done: item.done ?? 0,
		cancelled: item.cancelled ?? 0,
		updatedAt: item.updated_at,
		summary: item.description,
	}))
}

export async function loadTasks(projectId: string, projectSlug: string, projectName: string): Promise<Task[]> {
	const response = await request<{ items: Array<{ id: string; project_id: string; title: string; description: string; status: string; priority: string; assignee_id?: string | null; labels?: string[]; updated_at: string; parent_task_id?: string | null }> }>(`/projects/${projectId}/tasks`)
	const items = response.items
	return items.map((item) => ({
		id: item.id,
		title: item.title,
		project: projectSlug,
		projectName: projectName,
		status: item.status as Task['status'],
		priority: item.priority as Task['priority'],
		assignee: item.assignee_id ?? 'unassigned',
		labels: item.labels ?? [],
		summary: item.description,
		updatedAt: item.updated_at,
		parent: item.parent_task_id ?? undefined,
	}))
}

export async function loadTaskDetail(taskId: string, projectSlug: string, projectName: string): Promise<Task> {
	const item = await request<{
		id: string
		project_id: string
		title: string
		description: string
		status: string
		priority: string
		assignee_id?: string | null
		labels?: string[]
		updated_at: string
		parent_task_id?: string | null
		subtasks: Array<{ id: string; title: string }>
		comments: Array<{ id: string; body: string; updated_at: string }>
		activities: Array<{ id: string; label: string; kind: string; message: string; created_at: string; project_id?: string | null; task_id?: string | null }>
	}>(`/tasks/${taskId}`)
	return {
		id: item.id,
		title: item.title,
		project: projectSlug,
		projectName: projectName,
		status: item.status as Task['status'],
		priority: item.priority as Task['priority'],
		assignee: item.assignee_id ?? 'unassigned',
		labels: item.labels ?? [],
		summary: item.description,
		updatedAt: item.updated_at,
		parent: item.parent_task_id ?? undefined,
		subtasks: item.subtasks.map((subtask) => `${subtask.id} ${subtask.title}`),
		comments: item.comments.map((comment) => ({ id: comment.id, body: comment.body, updatedAt: comment.updated_at })),
		activities: item.activities.map((activity, index) => ({
			id: activity.id,
			title: activity.kind || activity.label,
			detail: activity.message,
			updatedAt: activity.created_at,
			sortKey: index,
			project: projectSlug,
			projectName,
			task: item.id,
			labels: [activity.label],
		})),
	}
}

export async function loadProjectOverview(projectId: string): Promise<{ project: Project; recentTasks: Task[]; recentSessions: Session[]; recentActivities: ActivityItem[] }> {
	const item = await request<{ project: { id: string; slug: string; title: string; description: string; open: number; in_progress: number; in_review: number; done: number; cancelled: number; updated_at: string }; recent_tasks: Array<{ id: string; project_id: string; title: string; description: string; status: string; priority: string; assignee_id?: string | null; labels?: string[]; updated_at: string; parent_task_id?: string | null }>; recent_sessions: Array<{ snapshot_id: string; title: string; description: string; artifact_name: string; artifact_path: string; updated_at: string }>; recent_activities: Array<{ id: string; label: string; kind: string; message: string; created_at: string; project_id?: string | null; task_id?: string | null }> }>(`/projects/${projectId}/overview`)
	return {
		project: {
			id: item.project.id,
			name: item.project.title,
			slug: item.project.slug,
			open: item.project.open ?? 0,
			inProgress: item.project.in_progress ?? 0,
			inReview: item.project.in_review ?? 0,
			done: item.project.done ?? 0,
			cancelled: item.project.cancelled ?? 0,
			updatedAt: item.project.updated_at,
			summary: item.project.description,
		},
		recentTasks: item.recent_tasks.map((task) => ({
			id: task.id,
			title: task.title,
			project: task.project_id,
			projectName: item.project.title,
			status: task.status as Task['status'],
			priority: task.priority as Task['priority'],
			assignee: task.assignee_id ?? 'unassigned',
			labels: task.labels ?? [],
			summary: task.description,
			updatedAt: task.updated_at,
			parent: task.parent_task_id ?? undefined,
		})),
		recentSessions: item.recent_sessions.map((session) => ({
			title: session.title,
			snapshotId: session.snapshot_id,
			description: session.description,
			artifactName: session.artifact_name,
			artifactPath: session.artifact_path,
			updatedAt: session.updated_at,
		})),
		recentActivities: item.recent_activities.map((activity, index) => ({
			id: activity.id,
			title: activity.kind || activity.label,
			detail: activity.message,
			updatedAt: activity.created_at,
			sortKey: index,
			project: activity.project_id ?? '',
			projectName: item.project.title,
			task: activity.task_id ?? '',
			labels: [activity.label],
		})),
	}
}

export async function loadSessions(): Promise<Session[]> {
	const response = await request<{ items: Array<{ snapshot_id: string; title: string; description: string; artifact_name: string; artifact_path: string; updated_at: string }> }>('/sessions')
	const items = response.items
	return items.map((item) => ({
		title: item.title,
		snapshotId: item.snapshot_id,
		description: item.description,
		artifactName: item.artifact_name,
		artifactPath: item.artifact_path,
		updatedAt: item.updated_at,
	}))
}

export async function loadActivities(query = ''): Promise<ActivityItem[]> {
	const response = await request<{ items: Array<{ id: string; label: string; kind: string; message: string; created_at: string; project_id?: string | null; task_id?: string | null }> }>(`/activities${query}`)
	const items = response.items
	return items.map((item, index) => ({
		id: item.id,
		title: item.kind || item.label,
		detail: item.message,
		updatedAt: item.created_at,
		sortKey: index,
		project: item.project_id ?? '',
		projectName: item.project_id ?? '',
		task: item.task_id ?? '',
		labels: [item.label],
	}))
}

export async function createTask(projectId: string, input: { title: string; description: string; priority: Task['priority']; labels: string[] }) {
	return requestJSON(`/projects/${projectId}/tasks`, {
		method: 'POST',
		body: JSON.stringify({
			title: input.title,
			description: input.description,
			status: 'backlog',
			priority: input.priority,
			labels: input.labels,
		}),
	})
}

export async function updateTask(taskId: string, input: { title: string; description: string; status: Task['status']; priority: Task['priority']; assignee: string; labels: string[] }) {
	const item = await requestJSON<{ id: string; project_id: string; title: string; description: string; status: string; priority: string; assignee_id?: string | null; labels?: string[]; updated_at: string; parent_task_id?: string | null }>(`/tasks/${taskId}`, {
		method: 'PATCH',
		body: JSON.stringify({
			title: input.title,
			description: input.description,
			status: input.status,
			priority: input.priority,
			assignee_id: input.assignee === 'unassigned' ? null : input.assignee,
			labels: input.labels,
		}),
	})
	return item
}

export async function uploadSessionArtifact(file: File): Promise<{ artifactName: string; artifactPath: string }> {
	const formData = new FormData()
	formData.set('file', file)
	const item = await requestForm<{ artifact_name: string; artifact_path: string }>('/sessions/uploads', formData)
	return {
		artifactName: item.artifact_name,
		artifactPath: item.artifact_path,
	}
}

export async function createSession(input: { title: string; description: string; artifactName: string; artifactPath: string; snapshotId?: string }) {
	return requestJSON('/sessions', {
		method: 'POST',
		body: JSON.stringify({
			snapshot_id: input.snapshotId,
			type: 'opencode',
			title: input.title,
			description: input.description,
			artifact_name: input.artifactName,
			artifact_path: input.artifactPath,
		}),
	})
}

export function sessionDownloadURL(snapshotId: string) {
	return `${API_BASE}/sessions/${snapshotId}/download`
}

export async function searchTasks(query: string): Promise<Task[]> {
	const response = await request<{ items: Array<{ id: string; project_id: string; title: string; description: string; status: string; priority: string; assignee_id?: string | null; labels?: string[]; updated_at: string; parent_task_id?: string | null }> }>(`/search?q=${encodeURIComponent(query)}`)
	const items = response.items
	return items.map((item) => ({
		id: item.id,
		title: item.title,
		project: item.project_id,
		projectName: item.project_id,
		status: item.status as Task['status'],
		priority: item.priority as Task['priority'],
		assignee: item.assignee_id ?? 'unassigned',
		labels: item.labels ?? [],
		summary: item.description,
		updatedAt: item.updated_at,
		parent: item.parent_task_id ?? undefined,
	}))
}
