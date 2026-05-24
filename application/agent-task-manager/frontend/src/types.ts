export type View = 'home' | 'projects' | 'tasks' | 'task-detail' | 'sessions' | 'activity'
export type TaskView = 'list' | 'board'
export type TaskStatus = 'backlog' | 'todo' | 'in_progress' | 'in_review'
export type Priority = 'P0' | 'P1' | 'P2'

export type Task = {
	id: string
	title: string
	project: string
	projectName: string
	status: TaskStatus
	priority: Priority
	assignee: string
	labels: string[]
	summary: string
	updatedAt: string
	parent?: string
	subtasks?: string[]
	acceptance?: string[]
	comments?: { id: string; body: string; updatedAt: string }[]
	activities?: ActivityItem[]
}

export type Project = {
	id: string
	name: string
	slug: string
	open: number
	inProgress: number
	inReview: number
	updatedAt: string
	summary: string
}

export type Session = {
	title: string
	snapshotId: string
	description: string
	artifactName: string
	artifactPath: string
	updatedAt: string
}

export type ActivityItem = {
	id: string
	title: string
	detail: string
	updatedAt: string
	sortKey: number
	project: string
	projectName: string
	task: string
	labels: string[]
}

export type PreviewState =
	| { kind: 'task'; taskId: string }
	| { kind: 'activity'; activityId: string }
	| { kind: 'session'; snapshotId: string }
	| null
