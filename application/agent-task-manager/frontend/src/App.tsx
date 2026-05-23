import { useMemo, useState } from 'react'

type View = 'home' | 'projects' | 'tasks' | 'sessions' | 'activity'
type TaskView = 'list' | 'board' | 'mine'
type TaskStatus = 'backlog' | 'todo' | 'in_progress' | 'in_review'
type Priority = 'P0' | 'P1' | 'P2'

type Task = {
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
}

type Project = {
	id: string
	name: string
	slug: string
	open: number
	inProgress: number
	inReview: number
	updatedAt: string
	summary: string
}

type Session = {
	title: string
	snapshotId: string
	description: string
	artifactName: string
	artifactPath: string
	updatedAt: string
}

type ActivityItem = {
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

type PreviewState =
	| { kind: 'task'; taskId: string }
	| { kind: 'activity'; activityId: string }
	| { kind: 'session'; snapshotId: string }
	| null

const palette = {
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

const navItems: { id: View; label: string; short: string }[] = [
	{ id: 'home', label: 'Home', short: 'HM' },
	{ id: 'projects', label: 'Projects', short: 'PR' },
	{ id: 'tasks', label: 'Tasks', short: 'TK' },
	{ id: 'sessions', label: 'Sessions', short: 'SN' },
	{ id: 'activity', label: 'Activity', short: 'AC' },
]

const taskViewItems: { id: TaskView; label: string }[] = [
	{ id: 'list', label: 'List' },
	{ id: 'board', label: 'Board' },
	{ id: 'mine', label: 'Mine' },
]

const projects: Project[] = [
	{
		id: 'PRJ-001',
		name: 'Agent Task Manager',
		slug: 'agent-task-manager',
		open: 24,
		inProgress: 7,
		inReview: 3,
		updatedAt: '2 min ago',
		summary: 'Go-based rebuild focused on agent-first task orchestration, explicit auditability, and durable session snapshots.',
	},
	{
		id: 'PRJ-002',
		name: 'OpenClaw Worker Orchestration',
		slug: 'openclaw-worker-orchestration',
		open: 11,
		inProgress: 4,
		inReview: 2,
		updatedAt: '17 min ago',
		summary: 'Dynamic OpenCode worker lifecycle, role packaging, and orchestration flow design.',
	},
	{
		id: 'PRJ-003',
		name: 'Pith Migration Research',
		slug: 'pith-migration-research',
		open: 7,
		inProgress: 1,
		inReview: 1,
		updatedAt: '46 min ago',
		summary: 'Feature mapping, migration gaps, and task-model constraints from the previous system.',
	},
]

const tasks: Task[] = [
	{
		id: 'ATM-101',
		title: 'Rebuild task domain in Go',
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		status: 'in_progress',
		priority: 'P0',
		assignee: 'builder-agent',
		labels: ['backend', 'domain'],
		summary: 'Replace the old TypeScript task model with Go-first services and persistence boundaries.',
		updatedAt: '12 min ago',
		parent: 'ATM-099 Build agent-task-manager backend MVP',
		subtasks: ['ATM-121 Define task repositories', 'ATM-122 Add task activity events'],
		acceptance: [
			'Task CRUD endpoints mapped to Go service boundaries.',
			'Repository interfaces do not leak HTTP concerns.',
			'Status transitions create audit log entries.',
		],
	},
	{
		id: 'ATM-102',
		title: 'Design agent-first CLI contracts',
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		status: 'todo',
		priority: 'P0',
		assignee: 'product-agent',
		labels: ['cli', 'agent'],
		summary: 'Define deterministic command outputs, JSON mode, and task/session workflows for automation.',
		updatedAt: '29 min ago',
	},
	{
		id: 'ATM-103',
		title: 'Refactor frontend shell around Tasks workspace',
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		status: 'in_review',
		priority: 'P1',
		assignee: 'ui-agent',
		labels: ['frontend', 'ux'],
		summary: 'Replace top-level demo tabs with a sidebar shell, top utility bar, and contextual preview panel.',
		updatedAt: '5 min ago',
	},
	{
		id: 'ATM-104',
		title: 'Add task reparenting support',
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		status: 'backlog',
		priority: 'P1',
		assignee: 'unassigned',
		labels: ['api', 'tasks'],
		summary: 'Support parent changes after task creation and surface them explicitly in the audit timeline.',
		updatedAt: '41 min ago',
	},
	{
		id: 'OCW-205',
		title: 'Launch product-role OpenCode worker',
		project: 'openclaw-worker-orchestration',
		projectName: 'OpenClaw Worker Orchestration',
		status: 'in_progress',
		priority: 'P0',
		assignee: 'product-role-worker',
		labels: ['openclaw', 'opencode'],
		summary: 'Provision a product-role worker with imported product and UI design skills.',
		updatedAt: '58 min ago',
	},
	{
		id: 'PMR-031',
		title: 'Capture parent-linking gap from pith',
		project: 'pith-migration-research',
		projectName: 'Pith Migration Research',
		status: 'todo',
		priority: 'P2',
		assignee: 'research-agent',
		labels: ['pith', 'task-model', 'ux'],
		summary: 'Record the limitation that parent linkage cannot be added after task creation.',
		updatedAt: '1 hr ago',
	},
]

const sessions: Session[] = [
	{
		title: 'OpenCode backend domain pass 1',
		snapshotId: 'b4f84a13-7d8e-4fd6-9a13-e9a7f94b3901',
		description: 'Snapshot imported after backend domain refactor checkpoint.',
		artifactName: 'ses_1bc5ca7a9ffeI2yl8CnNXcBxVk.json',
		artifactPath: 's3://agent-task-manager/sessions/opencode/ses_1bc5ca7a9ffeI2yl8CnNXcBxVk.json',
		updatedAt: '9 min ago',
	},
	{
		title: 'OpenCode board UX review snapshot',
		snapshotId: '0b72c993-0ae4-4d96-a21e-7e0ef6265830',
		description: 'Snapshot saved while evaluating board density and task card layout.',
		artifactName: 'ses_ux_board_review.json',
		artifactPath: '/data/snapshots/opencode/ses_ux_board_review.json',
		updatedAt: '37 min ago',
	},
	{
		title: 'OpenCode CLI contract draft snapshot',
		snapshotId: 'd8f720ce-1d1b-4556-b7bb-c2a52c9a65cb',
		description: 'Snapshot captured before revising agent CLI command contracts.',
		artifactName: 'ses_cli_contract_draft.json',
		artifactPath: 's3://agent-task-manager/sessions/opencode/ses_cli_contract_draft.json',
		updatedAt: '1 hr ago',
	},
]

const activities: ActivityItem[] = [
	{
		id: 'ACT-006',
		title: 'builder-agent started backend domain pass',
		detail: 'Task ATM-101 is being restructured around repositories, services, and activity events.',
		updatedAt: '2 min ago',
		sortKey: 6,
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		task: 'ATM-101',
		labels: ['backend', 'domain'],
	},
	{
		id: 'ACT-005',
		title: 'ui-agent moved ATM-103 to in_review',
		detail: 'The new shell, Tasks workspace, and preview panel are ready for product review.',
		updatedAt: '14 min ago',
		sortKey: 5,
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		task: 'ATM-103',
		labels: ['frontend', 'ux'],
	},
	{
		id: 'ACT-004',
		title: 'product-agent created ATM-104',
		detail: 'Task reparenting is now treated as a first-class requirement in the rebuilt system.',
		updatedAt: '27 min ago',
		sortKey: 4,
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		task: 'ATM-104',
		labels: ['api', 'tasks'],
	},
	{
		id: 'ACT-003',
		title: 'admin commented on ATM-101',
		detail: 'Parent changes should be explicit and audit-friendly in both API output and task detail.',
		updatedAt: '43 min ago',
		sortKey: 3,
		project: 'agent-task-manager',
		projectName: 'Agent Task Manager',
		task: 'ATM-101',
		labels: ['backend', 'tasks'],
	},
	{
		id: 'ACT-002',
		title: 'product-role worker task linked to initiative',
		detail: 'The product-role worker flow now lives as a proper subtask under the orchestration initiative.',
		updatedAt: '58 min ago',
		sortKey: 2,
		project: 'openclaw-worker-orchestration',
		projectName: 'OpenClaw Worker Orchestration',
		task: 'OCW-205',
		labels: ['openclaw', 'opencode', 'orchestration'],
	},
	{
		id: 'ACT-001',
		title: 'migration analysis captured parent-linking gap',
		detail: 'A task model limitation was recorded after finding that parent linkage could not be added after task creation.',
		updatedAt: '1 hr ago',
		sortKey: 1,
		project: 'pith-migration-research',
		projectName: 'Pith Migration Research',
		task: 'PMR-031',
		labels: ['pith', 'task-model', 'ux'],
	},
]

const statusMeta: Record<TaskStatus, { label: string; color: string; soft: string }> = {
	backlog: { label: 'Backlog', color: '#6f7e95', soft: '#eef1f6' },
	todo: { label: 'Todo', color: '#1c9bff', soft: '#e7f4ff' },
	in_progress: { label: 'In Progress', color: '#cc8a10', soft: '#fff4df' },
	in_review: { label: 'In Review', color: '#1f9d62', soft: '#e6f7ef' },
}

const priorityMeta: Record<Priority, { color: string; soft: string }> = {
	P0: { color: '#d64545', soft: '#ffe9e9' },
	P1: { color: '#cc8a10', soft: '#fff4df' },
	P2: { color: '#607089', soft: '#edf2f8' },
}

export default function App() {
	const [activeView, setActiveView] = useState<View>('tasks')
	const [taskView, setTaskView] = useState<TaskView>('list')
	const [selectedProject, setSelectedProject] = useState<string>('agent-task-manager')
	const [selectedTaskId, setSelectedTaskId] = useState<string>('ATM-101')
	const [preview, setPreview] = useState<PreviewState>({ kind: 'task', taskId: 'ATM-101' })
	const [taskFilterProject, setTaskFilterProject] = useState<string>('agent-task-manager')
	const [taskFilterLabel, setTaskFilterLabel] = useState<string>('')
	const [activityProject, setActivityProject] = useState<string>('')
	const [activityTask, setActivityTask] = useState<string>('')
	const [activityLabel, setActivityLabel] = useState<string>('')
	const [notProject, setNotProject] = useState(false)
	const [notTask, setNotTask] = useState(false)
	const [notLabel, setNotLabel] = useState(false)
	const [activitySort, setActivitySort] = useState<'asc' | 'desc'>('desc')

	const selectedTask = tasks.find((task) => task.id === selectedTaskId) ?? tasks[0]
	const activeProject = projects.find((project) => project.slug === selectedProject) ?? projects[0]

	const filteredTasks = useMemo(() => {
		return tasks.filter((task) => {
			if (taskFilterProject && task.project !== taskFilterProject) {
				return false
			}
			if (taskView === 'mine' && task.assignee !== 'builder-agent' && task.assignee !== 'ui-agent') {
				return false
			}
			if (taskFilterLabel && !task.labels.includes(taskFilterLabel)) {
				return false
			}
			return true
		})
	}, [taskFilterLabel, taskFilterProject, taskView])

	const filteredActivities = useMemo(() => {
		return [...activities]
			.filter((item) => {
				const projectMatch = !activityProject || item.project === activityProject
				const taskMatch = !activityTask || item.task === activityTask
				const labelMatch = !activityLabel || item.labels.includes(activityLabel)

				const projectPass = activityProject ? (notProject ? !projectMatch : projectMatch) : true
				const taskPass = activityTask ? (notTask ? !taskMatch : taskMatch) : true
				const labelPass = activityLabel ? (notLabel ? !labelMatch : labelMatch) : true

				return projectPass && taskPass && labelPass
			})
			.sort((left, right) => (activitySort === 'asc' ? left.sortKey - right.sortKey : right.sortKey - left.sortKey))
	}, [activityLabel, activityProject, activitySort, activityTask, notLabel, notProject, notTask])

	const taskProjects = Array.from(new Set(tasks.map((task) => task.project)))
	const taskLabels = Array.from(new Set(tasks.flatMap((task) => task.labels)))
	const activityProjects = Array.from(new Set(activities.map((item) => item.project)))
	const activityTasks = Array.from(new Set(activities.map((item) => item.task)))
	const activityLabels = Array.from(new Set(activities.flatMap((item) => item.labels)))

	const activeTaskFilters = [
		taskFilterProject ? `project:${taskFilterProject}` : '',
		taskFilterLabel ? `label:${taskFilterLabel}` : '',
	].filter(Boolean)

	const activeActivityFilters = [
		activityProject ? `${notProject ? 'NOT ' : ''}project:${activityProject}` : '',
		activityTask ? `${notTask ? 'NOT ' : ''}task:${activityTask}` : '',
		activityLabel ? `${notLabel ? 'NOT ' : ''}label:${activityLabel}` : '',
	].filter(Boolean)

	return (
		<div
			style={{
				minHeight: '100vh',
				background: palette.app,
				color: palette.text,
				fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
			}}
		>
			<div
				style={{
					display: 'grid',
					gridTemplateColumns: '260px minmax(0, 1fr)',
					minHeight: '100vh',
				}}
			>
				<aside
					style={{
						background: palette.sidebar,
						color: '#edf4ff',
						padding: 18,
						borderRight: '1px solid rgba(255,255,255,0.05)',
						display: 'flex',
						flexDirection: 'column',
						gap: 18,
					}}
				>
					<div style={{ display: 'grid', gap: 10 }}>
						<div style={{ color: '#7fd0ff', fontSize: 12, textTransform: 'uppercase', letterSpacing: 1.4 }}>Agent Task Manager</div>
						<div style={{ fontSize: 22, fontWeight: 800, lineHeight: 1.1 }}>Operational work shell for tasks, audit, and agent snapshots</div>
						<div style={{ color: '#91a0b8', fontSize: 13, lineHeight: 1.6 }}>
							Focused application shell with one stable navigation model and context-preserving detail flows.
						</div>
					</div>

					<nav style={{ display: 'grid', gap: 6 }}>
						{navItems.map((item) => {
							const active = activeView === item.id
							return (
								<button
									key={item.id}
									onClick={() => setActiveView(item.id)}
									style={{
										display: 'flex',
										alignItems: 'center',
										gap: 12,
										padding: '11px 12px',
										borderRadius: 12,
										border: active ? '1px solid rgba(127,208,255,0.45)' : '1px solid transparent',
										background: active ? 'rgba(28,155,255,0.16)' : 'transparent',
										color: active ? '#f6fbff' : '#b5c1d3',
										fontWeight: active ? 700 : 600,
										cursor: 'pointer',
									}}
								>
									<div
										style={{
											width: 28,
											height: 28,
											borderRadius: 9,
											background: active ? 'rgba(127,208,255,0.14)' : 'rgba(255,255,255,0.06)',
											display: 'grid',
											placeItems: 'center',
											fontSize: 11,
											letterSpacing: 0.3,
										}}
									>
										{item.short}
									</div>
									<span>{item.label}</span>
								</button>
							)
						})}
					</nav>

					<div style={{ marginTop: 'auto', display: 'grid', gap: 12 }}>
						<div style={sidebarInfoCardStyle}>
							<div style={sidebarLabelStyle}>Current Scope</div>
							<div style={{ fontWeight: 700 }}>{activeProject.name}</div>
							<div style={{ color: '#91a0b8', fontSize: 13 }}>{activeProject.slug}</div>
						</div>
						<div style={sidebarInfoCardStyle}>
							<div style={sidebarLabelStyle}>Operator</div>
							<div style={{ fontWeight: 700 }}>admin@atm.dev</div>
							<div style={{ color: '#91a0b8', fontSize: 13 }}>API auth and UI session states should surface explicitly.</div>
						</div>
					</div>
				</aside>

				<div style={{ background: palette.workspace, minWidth: 0 }}>
					<header
						style={{
							height: 72,
							padding: '0 22px',
							background: palette.topbar,
							borderBottom: '1px solid rgba(255,255,255,0.06)',
							display: 'grid',
							gridTemplateColumns: 'minmax(0, 1fr) auto auto',
							gap: 16,
							alignItems: 'center',
						}}
					>
						<div
							style={{
								background: 'rgba(255,255,255,0.06)',
								border: '1px solid rgba(255,255,255,0.08)',
								borderRadius: 12,
								padding: '11px 14px',
								color: '#d5deea',
								fontSize: 14,
							}}
						>
							Search tasks, comments, sessions, and activity
						</div>
						<div style={{ color: '#d3deeb', fontSize: 13 }}>Scope: {activeProject.slug}</div>
						<div style={{ display: 'flex', gap: 10 }}>
							<button style={topbarGhostButtonStyle}>Command</button>
							<button style={topbarPrimaryButtonStyle}>Quick Create</button>
						</div>
					</header>

					<div style={{ padding: 20 }}>
						{activeView === 'home' && (
							<HomeView
								project={activeProject}
								tasks={filteredTasks}
								activities={filteredActivities}
								sessions={sessions}
								onOpenTask={(taskId) => {
									setSelectedTaskId(taskId)
									setPreview({ kind: 'task', taskId })
									setActiveView('tasks')
								}}
							/>
						)}

						{activeView === 'projects' && (
							<ProjectsView
								projects={projects}
								selectedProject={selectedProject}
								onSelectProject={(slug) => {
									setSelectedProject(slug)
									setTaskFilterProject(slug)
									setActiveView('tasks')
								}}
							/>
						)}

						{activeView === 'tasks' && (
							<TasksWorkspace
								taskView={taskView}
								onChangeTaskView={setTaskView}
								selectedProject={taskFilterProject}
								onSelectProject={setTaskFilterProject}
								selectedLabel={taskFilterLabel}
								onSelectLabel={setTaskFilterLabel}
								projects={taskProjects}
								labels={taskLabels}
								filters={activeTaskFilters}
								tasks={filteredTasks}
								selectedTask={selectedTask}
								onSelectTask={(taskId) => {
									setSelectedTaskId(taskId)
									setPreview({ kind: 'task', taskId })
								}}
								onClearFilters={() => {
									setTaskFilterProject(selectedProject)
									setTaskFilterLabel('')
								}}
							/>
						)}

						{activeView === 'sessions' && (
							<SessionsView
								sessions={sessions}
								onPreview={(snapshotId) => setPreview({ kind: 'session', snapshotId })}
							/>
						)}

						{activeView === 'activity' && (
							<ActivityView
								activities={filteredActivities}
								projects={activityProjects}
								tasks={activityTasks}
								labels={activityLabels}
								project={activityProject}
								task={activityTask}
								label={activityLabel}
								notProject={notProject}
								notTask={notTask}
								notLabel={notLabel}
								sort={activitySort}
								onProjectChange={setActivityProject}
								onTaskChange={setActivityTask}
								onLabelChange={setActivityLabel}
								onToggleNotProject={() => setNotProject((value) => !value)}
								onToggleNotTask={() => setNotTask((value) => !value)}
								onToggleNotLabel={() => setNotLabel((value) => !value)}
								onToggleSort={() => setActivitySort((value) => (value === 'asc' ? 'desc' : 'asc'))}
								onPreview={(activityId) => setPreview({ kind: 'activity', activityId })}
								onClear={() => {
									setActivityProject('')
									setActivityTask('')
									setActivityLabel('')
									setNotProject(false)
									setNotTask(false)
									setNotLabel(false)
								}}
								activeFilters={activeActivityFilters}
							/>
						)}
					</div>
				</div>
			</div>

			{preview && (
				<PreviewPanel
					preview={preview}
					tasks={tasks}
					activities={activities}
					sessions={sessions}
					onClose={() => setPreview(null)}
					onOpenTaskDetail={(taskId) => {
						setSelectedTaskId(taskId)
						setActiveView('tasks')
					}}
				/>
			)}
		</div>
	)
}

function HomeView({
	project,
	tasks,
	activities,
	sessions,
	onOpenTask,
}: {
	project: Project
	tasks: Task[]
	activities: ActivityItem[]
	sessions: Session[]
	onOpenTask: (taskId: string) => void
}) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader
				eyebrow="Home"
				title="Current work, recent movement, and session checkpoints"
				description="A focused landing surface for operators. Counts are secondary to active work and recent changes."
			/>

			<div style={{ ...surfaceStyle, display: 'grid', gap: 14 }}>
				<div style={{ display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'center', flexWrap: 'wrap' }}>
					<div>
						<div style={eyebrowStyle}>Current Scope</div>
						<div style={{ fontSize: 24, fontWeight: 800, marginBottom: 6 }}>{project.name}</div>
						<div style={{ color: palette.muted, maxWidth: 720 }}>{project.summary}</div>
					</div>
					<div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, minmax(110px, 1fr))', gap: 10, minWidth: 360 }}>
						<CompactMetric label="Open" value={String(project.open)} />
						<CompactMetric label="In Progress" value={String(project.inProgress)} />
						<CompactMetric label="In Review" value={String(project.inReview)} />
					</div>
				</div>
			</div>

			<div style={{ display: 'grid', gridTemplateColumns: '1.3fr 1fr', gap: 18, alignItems: 'start' }}>
				<div style={{ display: 'grid', gap: 18 }}>
					<div style={surfaceStyle}>
						<SectionHeading title="My open tasks" subtitle="The first screen should answer what needs attention now." />
						<div style={{ display: 'grid', gap: 10 }}>
							{tasks.slice(0, 4).map((task) => (
								<button key={task.id} onClick={() => onOpenTask(task.id)} style={listButtonStyle}>
									<div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, marginBottom: 6 }}>
										<div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
											<IdPill value={task.id} />
											<StatusPill status={task.status} />
										</div>
										<div style={metaTextStyle}>{task.updatedAt}</div>
									</div>
									<div style={{ fontWeight: 700, marginBottom: 6 }}>{task.title}</div>
									<div style={{ color: palette.muted, lineHeight: 1.5 }}>{task.summary}</div>
								</button>
							))}
						</div>
					</div>

					<div style={surfaceStyle}>
						<SectionHeading title="Recent activity" subtitle="Audit-oriented updates with clear project and task context." />
						<div style={{ display: 'grid', gap: 12 }}>
							{activities.slice(0, 4).map((activity) => (
								<div key={activity.id} style={timelineRowStyle}>
									<div style={timelineDotStyle} />
									<div>
										<div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, marginBottom: 4 }}>
											<div style={{ fontWeight: 700 }}>{activity.title}</div>
											<div style={metaTextStyle}>{activity.updatedAt}</div>
										</div>
										<div style={{ color: palette.muted, lineHeight: 1.5, marginBottom: 8 }}>{activity.detail}</div>
										<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
											<MetaBadge label={activity.projectName} />
											<MetaBadge label={activity.task} />
										</div>
									</div>
								</div>
							))}
						</div>
					</div>
				</div>

				<div style={{ display: 'grid', gap: 18 }}>
					<div style={surfaceStyle}>
						<SectionHeading title="Recent sessions" subtitle="Snapshot registry entries with direct artifact awareness." />
						<div style={{ display: 'grid', gap: 10 }}>
							{sessions.map((session) => (
								<div key={session.snapshotId} style={registryCardStyle}>
									<div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, marginBottom: 6 }}>
										<div style={{ fontWeight: 700 }}>{session.title}</div>
										<div style={metaTextStyle}>{session.updatedAt}</div>
									</div>
									<div style={{ color: palette.muted, fontSize: 13, lineHeight: 1.5, marginBottom: 8 }}>{session.description}</div>
									<div style={{ fontFamily: 'ui-monospace, SFMono-Regular, monospace', fontSize: 12, color: palette.text }}>{session.artifactName}</div>
								</div>
							))}
						</div>
					</div>

					<div style={surfaceStyle}>
						<SectionHeading title="Design intent" subtitle="This shell favors compact work surfaces over display-first dashboard cards." />
						<div style={{ display: 'grid', gap: 8, color: palette.muted, lineHeight: 1.6 }}>
							<div>Tasks, sessions, and activity use one consistent surface model.</div>
							<div>Context flows into the preview panel instead of fragmenting into isolated pages.</div>
							<div>IDs, labels, timestamps, and states stay easy to scan and copy.</div>
						</div>
					</div>
				</div>
			</div>
		</div>
	)
}

function ProjectsView({
	projects,
	selectedProject,
	onSelectProject,
}: {
	projects: Project[]
	selectedProject: string
	onSelectProject: (slug: string) => void
}) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader
				eyebrow="Projects"
				title="Dense project index with fast scope switching"
				description="Projects use a table-first pattern instead of oversized card walls. Selecting a project should move directly into task work."
			/>

			<div style={surfaceStyle}>
				<div style={toolbarStyle}>
					<div style={toolbarTitleStyle}>Project Registry</div>
					<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
						<div style={ghostFieldStyle}>Search projects</div>
						<button style={ghostActionButtonStyle}>Sort: Updated</button>
					</div>
				</div>

				<div style={{ overflowX: 'auto' }}>
					<table style={tableStyle}>
						<thead>
							<tr>
								{['Project', 'Slug', 'Open', 'In Progress', 'In Review', 'Updated', 'Actions'].map((header) => (
									<th key={header} style={tableHeadStyle}>
										{header}
									</th>
								))}
							</tr>
						</thead>
						<tbody>
							{projects.map((project) => {
								const active = project.slug === selectedProject
								return (
									<tr key={project.id} style={{ background: active ? palette.accentSoft : 'transparent' }}>
										<td style={tableCellStyle}>
											<div style={{ fontWeight: 700 }}>{project.name}</div>
											<div style={{ color: palette.muted, fontSize: 13, marginTop: 4 }}>{project.summary}</div>
										</td>
										<td style={{ ...tableCellStyle, fontFamily: 'ui-monospace, SFMono-Regular, monospace' }}>{project.slug}</td>
										<td style={tableCellStyle}>{project.open}</td>
										<td style={tableCellStyle}>{project.inProgress}</td>
										<td style={tableCellStyle}>{project.inReview}</td>
										<td style={tableCellStyle}>{project.updatedAt}</td>
										<td style={tableCellStyle}>
											<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
												<button style={inlineActionButtonStyle} onClick={() => onSelectProject(project.slug)}>
													Open Tasks
												</button>
												<button style={inlineGhostActionButtonStyle}>Copy ID</button>
											</div>
										</td>
									</tr>
								)
							})}
						</tbody>
					</table>
				</div>
			</div>
		</div>
	)
}

function TasksWorkspace({
	taskView,
	onChangeTaskView,
	selectedProject,
	onSelectProject,
	selectedLabel,
	onSelectLabel,
	projects,
	labels,
	filters,
	tasks,
	selectedTask,
	onSelectTask,
	onClearFilters,
}: {
	taskView: TaskView
	onChangeTaskView: (view: TaskView) => void
	selectedProject: string
	onSelectProject: (project: string) => void
	selectedLabel: string
	onSelectLabel: (label: string) => void
	projects: string[]
	labels: string[]
	filters: string[]
	tasks: Task[]
	selectedTask: Task
	onSelectTask: (taskId: string) => void
	onClearFilters: () => void
}) {
	const grouped = taskView === 'board'
		? {
			backlog: tasks.filter((task) => task.status === 'backlog'),
			todo: tasks.filter((task) => task.status === 'todo'),
			in_progress: tasks.filter((task) => task.status === 'in_progress'),
			in_review: tasks.filter((task) => task.status === 'in_review'),
		}
		: null

	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader
				eyebrow="Tasks"
				title="Workspace for list, board, and task-detail flows"
				description="One workspace with internal view switching. The board is now a mode inside Tasks, not a separate top-level product section."
			/>

			<div style={surfaceStyle}>
				<div style={toolbarStyle}>
					<div style={{ display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap' }}>
						<div style={toolbarTitleStyle}>Tasks Workspace</div>
						<div style={scopePillStyle}>Scope: {selectedProject}</div>
					</div>
					<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
						{taskViewItems.map((item) => {
							const active = item.id === taskView
							return (
								<button
									key={item.id}
									onClick={() => onChangeTaskView(item.id)}
									style={{
										...segmentedButtonStyle,
										background: active ? palette.text : palette.panel,
										color: active ? '#ffffff' : palette.text,
										borderColor: active ? palette.text : palette.border,
									}}
								>
									{item.label}
								</button>
							)
						})}
						<button style={primaryActionButtonStyle}>Create Task</button>
					</div>
				</div>

				<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 14 }}>
					<select value={selectedProject} onChange={(event) => onSelectProject(event.target.value)} style={selectStyle}>
						{projects.map((project) => (
							<option key={project} value={project}>
								{project}
							</option>
						))}
					</select>
					<select value={selectedLabel} onChange={(event) => onSelectLabel(event.target.value)} style={selectStyle}>
						<option value="">All labels</option>
						{labels.map((label) => (
							<option key={label} value={label}>
								{label}
							</option>
						))}
					</select>
					<button style={ghostActionButtonStyle}>Filter rail</button>
					{filters.length > 0 && (
						<>
							{filters.map((filter) => (
								<FilterChip key={filter} label={filter} />
							))}
							<button style={clearActionButtonStyle} onClick={onClearFilters}>
								Clear filters
							</button>
						</>
					)}
				</div>

				{tasks.length === 0 ? (
					<EmptyState
						title="No tasks match this scope"
						detail="The current project or label filters removed all visible tasks. Clear filters or switch scope to continue."
					/>
				) : taskView === 'board' && grouped ? (
					<div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(260px, 1fr))', gap: 14, overflowX: 'auto' }}>
						{Object.entries(grouped).map(([key, columnTasks]) => (
							<div key={key} style={boardColumnStyle}>
								<div style={boardColumnHeaderStyle}>
									<span>{statusMeta[key as TaskStatus].label}</span>
									<span style={metaTextStyle}>{columnTasks.length}</span>
								</div>
								<div style={{ display: 'grid', gap: 10 }}>
									{columnTasks.map((task) => (
										<button key={task.id} onClick={() => onSelectTask(task.id)} style={boardCardStyle}>
											<div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, marginBottom: 8 }}>
												<IdPill value={task.id} />
												<PriorityPill priority={task.priority} />
											</div>
											<div style={{ fontWeight: 700, lineHeight: 1.35, marginBottom: 8 }}>{task.title}</div>
											<div style={{ color: palette.muted, fontSize: 13, lineHeight: 1.5, marginBottom: 10 }}>{task.summary}</div>
											<div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 8 }}>
												{task.labels.slice(0, 2).map((label) => (
													<LabelChip key={label} label={label} />
												))}
											</div>
											<div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, color: palette.subtle, fontSize: 12 }}>
												<span>{task.assignee}</span>
												<span>{task.updatedAt}</span>
											</div>
										</button>
									))}
								</div>
							</div>
						))}
					</div>
				) : (
					<div style={{ overflowX: 'auto' }}>
						<table style={tableStyle}>
							<thead>
								<tr>
									{['ID', 'Title', 'Status', 'Priority', 'Labels', 'Assignee', 'Updated'].map((header) => (
										<th key={header} style={tableHeadStyle}>
											{header}
										</th>
									))}
								</tr>
							</thead>
							<tbody>
								{tasks.map((task) => {
									const active = selectedTask.id === task.id
									return (
										<tr key={task.id} onClick={() => onSelectTask(task.id)} style={{ cursor: 'pointer', background: active ? palette.accentSoft : 'transparent' }}>
											<td style={{ ...tableCellStyle, fontFamily: 'ui-monospace, SFMono-Regular, monospace' }}>{task.id}</td>
											<td style={tableCellStyle}>
												<div style={{ fontWeight: 700 }}>{task.title}</div>
												<div style={{ color: palette.muted, fontSize: 13, marginTop: 4 }}>{task.summary}</div>
											</td>
											<td style={tableCellStyle}>
												<StatusPill status={task.status} />
											</td>
											<td style={tableCellStyle}>
												<PriorityPill priority={task.priority} />
											</td>
											<td style={tableCellStyle}>
												<div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
													{task.labels.map((label) => (
														<LabelChip key={label} label={label} />
													))}
												</div>
											</td>
											<td style={tableCellStyle}>{task.assignee}</td>
											<td style={tableCellStyle}>{task.updatedAt}</td>
										</tr>
									)
								})}
							</tbody>
						</table>
					</div>
				)}

				<div style={{ marginTop: 18 }}>
					<TaskDetail task={selectedTask} compact />
				</div>
			</div>
		</div>
	)
}

function SessionsView({ sessions, onPreview }: { sessions: Session[]; onPreview: (snapshotId: string) => void }) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader
				eyebrow="Sessions"
				title="Registry for imported agent session snapshots"
				description="A document-registry mental model: upload, register, inspect metadata, and download artifacts without pretending these are task cards."
			/>

			<div style={surfaceStyle}>
				<div style={toolbarStyle}>
					<div style={toolbarTitleStyle}>Snapshot Registry</div>
					<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
						<button style={primaryActionButtonStyle}>Upload Artifact</button>
						<button style={ghostActionButtonStyle}>Register Existing Path</button>
					</div>
				</div>

				<div style={{ overflowX: 'auto' }}>
					<table style={tableStyle}>
						<thead>
							<tr>
								{['Title', 'Snapshot ID', 'Description', 'Artifact Name', 'Artifact Path', 'Updated', 'Actions'].map((header) => (
									<th key={header} style={tableHeadStyle}>
										{header}
									</th>
								))}
							</tr>
						</thead>
						<tbody>
							{sessions.map((session) => (
								<tr key={session.snapshotId}>
									<td style={tableCellStyle}>
										<div style={{ fontWeight: 700 }}>{session.title}</div>
									</td>
									<td style={{ ...tableCellStyle, fontFamily: 'ui-monospace, SFMono-Regular, monospace', fontSize: 12 }}>{session.snapshotId}</td>
									<td style={tableCellStyle}>{session.description}</td>
									<td style={tableCellStyle}>{session.artifactName}</td>
									<td style={{ ...tableCellStyle, maxWidth: 280 }}>
										<div style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', fontFamily: 'ui-monospace, SFMono-Regular, monospace', fontSize: 12 }}>
											{session.artifactPath}
										</div>
									</td>
									<td style={tableCellStyle}>{session.updatedAt}</td>
									<td style={tableCellStyle}>
										<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
											<button style={inlineActionButtonStyle} onClick={() => onPreview(session.snapshotId)}>
												Preview
											</button>
											<button style={inlineGhostActionButtonStyle}>Download</button>
										</div>
									</td>
								</tr>
							))}
						</tbody>
					</table>
				</div>
			</div>
		</div>
	)
}

function ActivityView({
	activities,
	projects,
	tasks,
	labels,
	project,
	task,
	label,
	notProject,
	notTask,
	notLabel,
	sort,
	onProjectChange,
	onTaskChange,
	onLabelChange,
	onToggleNotProject,
	onToggleNotTask,
	onToggleNotLabel,
	onToggleSort,
	onPreview,
	onClear,
	activeFilters,
}: {
	activities: ActivityItem[]
	projects: string[]
	tasks: string[]
	labels: string[]
	project: string
	task: string
	label: string
	notProject: boolean
	notTask: boolean
	notLabel: boolean
	sort: 'asc' | 'desc'
	onProjectChange: (value: string) => void
	onTaskChange: (value: string) => void
	onLabelChange: (value: string) => void
	onToggleNotProject: () => void
	onToggleNotTask: () => void
	onToggleNotLabel: () => void
	onToggleSort: () => void
	onPreview: (activityId: string) => void
	onClear: () => void
	activeFilters: string[]
}) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader
				eyebrow="Activity"
				title="Global audit timeline with explicit combined filters"
				description="Project, task, and label filters share one visible filter model with removable chips, NOT state, and explicit sort direction."
			/>

			<div style={surfaceStyle}>
				<div style={toolbarStyle}>
					<div style={toolbarTitleStyle}>Activity Timeline</div>
					<button style={ghostActionButtonStyle} onClick={onToggleSort}>
						Sort: {sort === 'asc' ? 'Ascending' : 'Descending'}
					</button>
				</div>

				<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
					<select value={project} onChange={(event) => onProjectChange(event.target.value)} style={selectStyle}>
						<option value="">Project</option>
						{projects.map((item) => (
							<option key={item} value={item}>
								{item}
							</option>
						))}
					</select>
					<ToggleChip label="NOT" active={notProject} onClick={onToggleNotProject} />

					<select value={task} onChange={(event) => onTaskChange(event.target.value)} style={selectStyle}>
						<option value="">Task</option>
						{tasks.map((item) => (
							<option key={item} value={item}>
								{item}
							</option>
						))}
					</select>
					<ToggleChip label="NOT" active={notTask} onClick={onToggleNotTask} />

					<select value={label} onChange={(event) => onLabelChange(event.target.value)} style={selectStyle}>
						<option value="">Label</option>
						{labels.map((item) => (
							<option key={item} value={item}>
								{item}
							</option>
						))}
					</select>
					<ToggleChip label="NOT" active={notLabel} onClick={onToggleNotLabel} />

					{activeFilters.map((filter) => (
						<FilterChip key={filter} label={filter} />
					))}
					{activeFilters.length > 0 && (
						<button style={clearActionButtonStyle} onClick={onClear}>
							Clear all
						</button>
					)}
				</div>

				{activities.length === 0 ? (
					<EmptyState
						title="No activity matches this filter set"
						detail="The current combined filter state removed all timeline rows. Clear filters or invert one condition to continue."
					/>
				) : (
					<div style={{ display: 'grid', gap: 12 }}>
						{activities.map((activity) => (
							<button key={activity.id} onClick={() => onPreview(activity.id)} style={timelineButtonStyle}>
								<div style={timelineDotStyle} />
								<div style={{ minWidth: 0 }}>
									<div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, marginBottom: 4 }}>
										<div style={{ fontWeight: 700 }}>{activity.title}</div>
										<div style={metaTextStyle}>{activity.updatedAt}</div>
									</div>
									<div style={{ color: palette.muted, lineHeight: 1.55, marginBottom: 8 }}>{activity.detail}</div>
									<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
										<MetaBadge label={activity.projectName} />
										<MetaBadge label={activity.task} />
										{activity.labels.map((item) => (
											<LabelChip key={item} label={item} />
										))}
									</div>
								</div>
							</button>
						))}
					</div>
				)}
			</div>
		</div>
	)
}

function TaskDetail({ task, compact = false }: { task: Task; compact?: boolean }) {
	return (
		<div style={{ ...surfaceStyle, padding: compact ? 18 : 22, gap: 0 }}>
			<div style={{ display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-start', flexWrap: 'wrap', marginBottom: 16 }}>
				<div>
					<div style={eyebrowStyle}>Task Detail</div>
					<div style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap', marginBottom: 8 }}>
						<div style={{ fontSize: compact ? 22 : 28, fontWeight: 800, lineHeight: 1.15 }}>{task.title}</div>
						<IdPill value={task.id} />
					</div>
					<div style={{ color: palette.muted, maxWidth: 760, lineHeight: 1.6 }}>{task.summary}</div>
				</div>
				<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
					<StatusPill status={task.status} />
					<PriorityPill priority={task.priority} />
					<MetaBadge label={task.assignee} />
					<button style={inlineGhostActionButtonStyle}>Copy ID</button>
				</div>
			</div>

			<div style={{ display: 'grid', gridTemplateColumns: compact ? '1fr 1fr' : '1.2fr 1fr', gap: 16, marginBottom: 16 }}>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Description and metadata</div>
					<div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 12 }}>
						Task structure, labels, assignee state, and activity should stay explicit near the top of the detail view.
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
						{task.labels.map((label) => (
							<LabelChip key={label} label={label} />
						))}
					</div>
				</div>

				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Parent and subtask structure</div>
					<div style={{ display: 'grid', gap: 10 }}>
						<div>
							<div style={metaLabelStyle}>Parent</div>
							<div style={{ fontWeight: 700 }}>{task.parent ?? 'No parent task'}</div>
						</div>
						<div>
							<div style={metaLabelStyle}>Subtasks</div>
							<div style={{ display: 'grid', gap: 6 }}>
								{task.subtasks?.map((subtask) => (
									<div key={subtask} style={subtaskRowStyle}>
										{subtask}
									</div>
								)) ?? <div style={{ color: palette.muted }}>No subtasks</div>}
							</div>
						</div>
					</div>
				</div>
			</div>

			<div style={{ display: 'grid', gridTemplateColumns: compact ? '1fr 1fr' : '1fr 1fr', gap: 16 }}>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Comments</div>
					<div style={{ display: 'grid', gap: 10 }}>
						<div style={commentRowStyle}>
							<div style={{ fontWeight: 700, marginBottom: 4 }}>admin</div>
							<div style={{ color: palette.muted, lineHeight: 1.55 }}>Parent changes should be explicit in both API responses and UI detail surfaces.</div>
						</div>
						<div style={commentRowStyle}>
							<div style={{ fontWeight: 700, marginBottom: 4 }}>builder-agent</div>
							<div style={{ color: palette.muted, lineHeight: 1.55 }}>Repository interfaces now isolate storage concerns from transport and service logic.</div>
						</div>
					</div>
				</div>

				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Recent activity</div>
					<div style={{ display: 'grid', gap: 10 }}>
						{task.acceptance?.map((item) => (
							<div key={item} style={activityNoteStyle}>
								{item}
							</div>
						))}
					</div>
				</div>
			</div>
		</div>
	)
}

function PreviewPanel({
	preview,
	tasks,
	activities,
	sessions,
	onClose,
	onOpenTaskDetail,
}: {
	preview: PreviewState
	tasks: Task[]
	activities: ActivityItem[]
	sessions: Session[]
	onClose: () => void
	onOpenTaskDetail: (taskId: string) => void
}) {
	const content = (() => {
		if (preview?.kind === 'task') {
			const task = tasks.find((item) => item.id === preview.taskId)
			if (!task) {
				return null
			}
			return (
				<>
					<div style={previewHeaderStyle}>
						<div>
							<div style={eyebrowStyle}>Task Preview</div>
							<div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{task.title}</div>
						</div>
						<button style={closeButtonStyle} onClick={onClose}>
							Close
						</button>
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 12 }}>
						<IdPill value={task.id} />
						<StatusPill status={task.status} />
						<PriorityPill priority={task.priority} />
					</div>
					<div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 14 }}>{task.summary}</div>
					<div style={{ display: 'grid', gap: 8, marginBottom: 16 }}>
						<PreviewMeta label="Project" value={task.projectName} />
						<PreviewMeta label="Assignee" value={task.assignee} />
						<PreviewMeta label="Updated" value={task.updatedAt} />
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 16 }}>
						{task.labels.map((label) => (
							<LabelChip key={label} label={label} />
						))}
					</div>
					<button style={primaryActionButtonStyle} onClick={() => onOpenTaskDetail(task.id)}>
						Open Full Detail
					</button>
				</>
			)
		}

		if (preview?.kind === 'activity') {
			const activity = activities.find((item) => item.id === preview.activityId)
			if (!activity) {
				return null
			}
			return (
				<>
					<div style={previewHeaderStyle}>
						<div>
							<div style={eyebrowStyle}>Activity Preview</div>
							<div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{activity.title}</div>
						</div>
						<button style={closeButtonStyle} onClick={onClose}>
							Close
						</button>
					</div>
					<div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 16 }}>{activity.detail}</div>
					<div style={{ display: 'grid', gap: 8, marginBottom: 14 }}>
						<PreviewMeta label="Project" value={activity.projectName} />
						<PreviewMeta label="Task" value={activity.task} />
						<PreviewMeta label="Updated" value={activity.updatedAt} />
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
						{activity.labels.map((label) => (
							<LabelChip key={label} label={label} />
						))}
					</div>
				</>
			)
		}

		if (preview?.kind === 'session') {
			const session = sessions.find((item) => item.snapshotId === preview.snapshotId)
			if (!session) {
				return null
			}
			return (
				<>
					<div style={previewHeaderStyle}>
						<div>
							<div style={eyebrowStyle}>Session Preview</div>
							<div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{session.title}</div>
						</div>
						<button style={closeButtonStyle} onClick={onClose}>
							Close
						</button>
					</div>
					<div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 16 }}>{session.description}</div>
					<div style={{ display: 'grid', gap: 8, marginBottom: 14 }}>
						<PreviewMeta label="Snapshot ID" value={session.snapshotId} mono />
						<PreviewMeta label="Artifact Name" value={session.artifactName} mono />
						<PreviewMeta label="Artifact Path" value={session.artifactPath} mono />
					</div>
					<div style={{ display: 'flex', gap: 8 }}>
						<button style={primaryActionButtonStyle}>Download Artifact</button>
						<button style={ghostActionButtonStyle}>Copy Path</button>
					</div>
				</>
			)
		}

		return null
	})()

	if (!content) {
		return null
	}

	return (
		<div
			style={{
				position: 'fixed',
				top: 24,
				right: 24,
				width: 380,
				maxWidth: 'calc(100vw - 48px)',
				background: palette.panel,
				border: `1px solid ${palette.border}`,
				borderRadius: 18,
				boxShadow: palette.shadow,
				padding: 18,
				zIndex: 30,
			}}
		>
			{content}
		</div>
	)
}

function WorkspaceHeader({ eyebrow, title, description }: { eyebrow: string; title: string; description: string }) {
	return (
		<div style={{ display: 'grid', gap: 6 }}>
			<div style={eyebrowStyle}>{eyebrow}</div>
			<h1 style={{ margin: 0, fontSize: 30, lineHeight: 1.12 }}>{title}</h1>
			<div style={{ color: palette.muted, lineHeight: 1.6, maxWidth: 900 }}>{description}</div>
		</div>
	)
}

function SectionHeading({ title, subtitle }: { title: string; subtitle: string }) {
	return (
		<div style={{ marginBottom: 14 }}>
			<div style={{ fontWeight: 800, fontSize: 18, marginBottom: 4 }}>{title}</div>
			<div style={{ color: palette.muted, lineHeight: 1.55 }}>{subtitle}</div>
		</div>
	)
}

function CompactMetric({ label, value }: { label: string; value: string }) {
	return (
		<div style={{ border: `1px solid ${palette.border}`, borderRadius: 14, background: palette.panel, padding: 14 }}>
			<div style={metaLabelStyle}>{label}</div>
			<div style={{ fontSize: 24, fontWeight: 800, marginTop: 6 }}>{value}</div>
		</div>
	)
}

function StatusPill({ status }: { status: TaskStatus }) {
	const meta = statusMeta[status]
	return <Pill label={meta.label} color={meta.color} background={meta.soft} />
}

function PriorityPill({ priority }: { priority: Priority }) {
	const meta = priorityMeta[priority]
	return <Pill label={priority} color={meta.color} background={meta.soft} />
}

function LabelChip({ label }: { label: string }) {
	return <Pill label={label} color={palette.chipText} background={palette.chip} />
}

function MetaBadge({ label }: { label: string }) {
	return <Pill label={label} color={palette.muted} background={palette.panelAlt} />
}

function FilterChip({ label }: { label: string }) {
	return <Pill label={label} color={palette.accentStrong} background={palette.accentSoft} />
}

function ToggleChip({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
	return (
		<button
			onClick={onClick}
			style={{
				borderRadius: 999,
				padding: '8px 11px',
				border: `1px solid ${active ? palette.text : palette.border}`,
				background: active ? palette.text : palette.panel,
				color: active ? '#fff' : palette.text,
				fontWeight: 700,
				cursor: 'pointer',
			}}
		>
			{label}
		</button>
	)
}

function Pill({ label, color, background }: { label: string; color: string; background: string }) {
	return (
		<span
			style={{
				display: 'inline-flex',
				alignItems: 'center',
				padding: '5px 9px',
				borderRadius: 999,
				background,
				color,
				fontSize: 12,
				fontWeight: 700,
			}}
		>
			{label}
		</span>
	)
}

function IdPill({ value }: { value: string }) {
	return (
		<span
			style={{
				display: 'inline-flex',
				alignItems: 'center',
				padding: '5px 9px',
				borderRadius: 999,
				background: palette.panelAlt,
				color: palette.text,
				fontSize: 12,
				fontWeight: 700,
				fontFamily: 'ui-monospace, SFMono-Regular, monospace',
			}}
		>
			{value}
		</span>
	)
}

function PreviewMeta({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
	return (
		<div>
			<div style={metaLabelStyle}>{label}</div>
			<div style={{ fontWeight: 700, fontFamily: mono ? 'ui-monospace, SFMono-Regular, monospace' : undefined, wordBreak: 'break-word' }}>{value}</div>
		</div>
	)
}

function EmptyState({ title, detail }: { title: string; detail: string }) {
	return (
		<div
			style={{
				border: `1px dashed ${palette.borderStrong}`,
				borderRadius: 16,
				background: palette.panelAlt,
				padding: 28,
				textAlign: 'center',
			}}
		>
			<div style={{ fontWeight: 800, fontSize: 18, marginBottom: 6 }}>{title}</div>
			<div style={{ color: palette.muted, lineHeight: 1.6, maxWidth: 600, margin: '0 auto' }}>{detail}</div>
		</div>
	)
}

const surfaceStyle: React.CSSProperties = {
	background: palette.panel,
	border: `1px solid ${palette.border}`,
	borderRadius: 18,
	padding: 18,
	boxShadow: palette.shadow,
}

const toolbarStyle: React.CSSProperties = {
	display: 'flex',
	justifyContent: 'space-between',
	gap: 14,
	alignItems: 'center',
	flexWrap: 'wrap',
	marginBottom: 14,
}

const toolbarTitleStyle: React.CSSProperties = {
	fontWeight: 800,
	fontSize: 18,
}

const sidebarInfoCardStyle: React.CSSProperties = {
	border: '1px solid rgba(255,255,255,0.07)',
	borderRadius: 14,
	padding: 12,
	background: 'rgba(255,255,255,0.03)',
	lineHeight: 1.5,
}

const sidebarLabelStyle: React.CSSProperties = {
	fontSize: 11,
	textTransform: 'uppercase',
	letterSpacing: 1,
	color: '#7f90a9',
	marginBottom: 6,
}

const topbarPrimaryButtonStyle: React.CSSProperties = {
	border: 'none',
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.accent,
	color: '#061422',
	fontWeight: 800,
	cursor: 'pointer',
}

const topbarGhostButtonStyle: React.CSSProperties = {
	border: '1px solid rgba(255,255,255,0.1)',
	borderRadius: 12,
	padding: '10px 14px',
	background: 'rgba(255,255,255,0.04)',
	color: '#d9e5f2',
	fontWeight: 700,
	cursor: 'pointer',
}

const primaryActionButtonStyle: React.CSSProperties = {
	border: 'none',
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.accent,
	color: '#08111f',
	fontWeight: 800,
	cursor: 'pointer',
}

const ghostActionButtonStyle: React.CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.panel,
	color: palette.text,
	fontWeight: 700,
	cursor: 'pointer',
}

const inlineActionButtonStyle: React.CSSProperties = {
	border: 'none',
	borderRadius: 10,
	padding: '8px 10px',
	background: palette.accentSoft,
	color: palette.accentStrong,
	fontWeight: 700,
	cursor: 'pointer',
}

const inlineGhostActionButtonStyle: React.CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 10,
	padding: '8px 10px',
	background: palette.panel,
	color: palette.text,
	fontWeight: 700,
	cursor: 'pointer',
}

const clearActionButtonStyle: React.CSSProperties = {
	border: 'none',
	background: 'transparent',
	color: palette.accentStrong,
	fontWeight: 700,
	cursor: 'pointer',
	padding: '8px 4px',
}

const segmentedButtonStyle: React.CSSProperties = {
	borderRadius: 10,
	padding: '9px 12px',
	border: `1px solid ${palette.border}`,
	fontWeight: 700,
	cursor: 'pointer',
}

const selectStyle: React.CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 12,
	padding: '10px 12px',
	background: palette.panel,
	color: palette.text,
	minWidth: 150,
}

const scopePillStyle: React.CSSProperties = {
	borderRadius: 999,
	padding: '6px 10px',
	background: palette.panelAlt,
	color: palette.muted,
	fontWeight: 700,
	fontSize: 12,
}

const tableStyle: React.CSSProperties = {
	width: '100%',
	borderCollapse: 'collapse',
	minWidth: 920,
}

const tableHeadStyle: React.CSSProperties = {
	textAlign: 'left',
	padding: '10px 12px',
	fontSize: 12,
	textTransform: 'uppercase',
	letterSpacing: 0.7,
	color: palette.subtle,
	borderBottom: `1px solid ${palette.border}`,
}

const tableCellStyle: React.CSSProperties = {
	padding: '13px 12px',
	borderBottom: `1px solid ${palette.border}`,
	verticalAlign: 'top',
}

const metaTextStyle: React.CSSProperties = {
	color: palette.subtle,
	fontSize: 12,
}

const metaLabelStyle: React.CSSProperties = {
	color: palette.subtle,
	fontSize: 12,
	textTransform: 'uppercase',
	letterSpacing: 0.6,
	marginBottom: 4,
}

const eyebrowStyle: React.CSSProperties = {
	color: palette.accentStrong,
	fontSize: 12,
	textTransform: 'uppercase',
	letterSpacing: 1.2,
	fontWeight: 800,
}

const ghostFieldStyle: React.CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.panel,
	color: palette.subtle,
	minWidth: 200,
}

const listButtonStyle: React.CSSProperties = {
	textAlign: 'left',
	border: `1px solid ${palette.border}`,
	background: palette.panel,
	borderRadius: 14,
	padding: 14,
	color: palette.text,
	cursor: 'pointer',
}

const timelineRowStyle: React.CSSProperties = {
	display: 'grid',
	gridTemplateColumns: '12px minmax(0, 1fr)',
	gap: 10,
	alignItems: 'start',
}

const timelineButtonStyle: React.CSSProperties = {
	textAlign: 'left',
	border: `1px solid ${palette.border}`,
	background: palette.panel,
	borderRadius: 14,
	padding: 14,
	display: 'grid',
	gridTemplateColumns: '12px minmax(0, 1fr)',
	gap: 10,
	alignItems: 'start',
	cursor: 'pointer',
}

const timelineDotStyle: React.CSSProperties = {
	width: 10,
	height: 10,
	borderRadius: 999,
	background: palette.accent,
	marginTop: 7,
}

const registryCardStyle: React.CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 14,
	padding: 14,
	background: palette.panelAlt,
}

const boardColumnStyle: React.CSSProperties = {
	background: palette.panelAlt,
	border: `1px solid ${palette.border}`,
	borderRadius: 16,
	padding: 12,
	minWidth: 260,
	alignSelf: 'start',
}

const boardColumnHeaderStyle: React.CSSProperties = {
	display: 'flex',
	justifyContent: 'space-between',
	alignItems: 'center',
	position: 'sticky',
	top: 0,
	paddingBottom: 10,
	marginBottom: 10,
	fontWeight: 800,
	borderBottom: `1px solid ${palette.border}`,
	background: palette.panelAlt,
}

const boardCardStyle: React.CSSProperties = {
	textAlign: 'left',
	border: `1px solid ${palette.border}`,
	background: palette.panel,
	borderRadius: 14,
	padding: 12,
	color: palette.text,
	cursor: 'pointer',
}

const detailBlockStyle: React.CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 14,
	background: palette.panelAlt,
	padding: 14,
}

const detailBlockTitleStyle: React.CSSProperties = {
	fontWeight: 800,
	marginBottom: 10,
}

const subtaskRowStyle: React.CSSProperties = {
	borderRadius: 10,
	padding: '10px 12px',
	background: palette.panel,
	border: `1px solid ${palette.border}`,
	fontWeight: 600,
}

const commentRowStyle: React.CSSProperties = {
	borderRadius: 12,
	padding: '12px 13px',
	background: palette.panel,
	border: `1px solid ${palette.border}`,
}

const activityNoteStyle: React.CSSProperties = {
	borderRadius: 12,
	padding: '12px 13px',
	background: palette.panel,
	border: `1px solid ${palette.border}`,
	color: palette.muted,
	lineHeight: 1.55,
}

const previewHeaderStyle: React.CSSProperties = {
	display: 'flex',
	justifyContent: 'space-between',
	gap: 12,
	alignItems: 'flex-start',
	marginBottom: 14,
}

const closeButtonStyle: React.CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 10,
	padding: '8px 10px',
	background: palette.panel,
	color: palette.text,
	cursor: 'pointer',
	fontWeight: 700,
}
