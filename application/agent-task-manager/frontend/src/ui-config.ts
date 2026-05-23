import type { Priority, TaskStatus, TaskView, View } from './types'

export const palette = {
	app: '#0a0d12',
	sidebar: '#0f1319',
	topbar: '#121821',
	workspace: '#f3f6fb',
	panel: '#ffffff',
	panelAlt: '#f8fbff',
	border: '#d8e0eb',
	borderStrong: '#bcc8d8',
	text: '#142033',
	muted: '#607089',
	subtle: '#8593a8',
	accent: '#1c9bff',
	accentSoft: '#e8f5ff',
	accentStrong: '#0e6fcb',
	success: '#1f9d62',
	warning: '#cc8a10',
	danger: '#d64545',
	chip: '#edf2f8',
	chipText: '#42516a',
	shadow: '0 10px 26px rgba(10, 16, 24, 0.08)',
} as const

export const navItems: { id: View; label: string; short: string }[] = [
	{ id: 'home', label: 'Home', short: 'HM' },
	{ id: 'projects', label: 'Projects', short: 'PR' },
	{ id: 'tasks', label: 'Tasks', short: 'TK' },
	{ id: 'sessions', label: 'Sessions', short: 'SN' },
	{ id: 'activity', label: 'Activity', short: 'AC' },
]

export const taskViewItems: { id: TaskView; label: string }[] = [
	{ id: 'list', label: 'List' },
	{ id: 'board', label: 'Board' },
	{ id: 'mine', label: 'Mine' },
]

export const statusMeta: Record<TaskStatus, { label: string; color: string; soft: string }> = {
	backlog: { label: 'Backlog', color: '#6f7e95', soft: '#eef1f6' },
	todo: { label: 'Todo', color: '#1c9bff', soft: '#e7f4ff' },
	in_progress: { label: 'In Progress', color: '#cc8a10', soft: '#fff4df' },
	in_review: { label: 'In Review', color: '#1f9d62', soft: '#e6f7ef' },
}

export const priorityMeta: Record<Priority, { color: string; soft: string }> = {
	P0: { color: '#d64545', soft: '#ffe9e9' },
	P1: { color: '#cc8a10', soft: '#fff4df' },
	P2: { color: '#607089', soft: '#edf2f8' },
}
