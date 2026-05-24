import { useEffect, useMemo, useRef, useState } from 'react'

import { createSession, createTask, loadActivities, loadProjectOverview, loadProjects, loadSessions, loadTaskDetail, loadTasks, searchTasks, sessionDownloadURL, updateTask, uploadSessionArtifact } from './api'
import { navItems } from './ui-config'
import { palette, sidebarInfoCardStyle, sidebarLabelStyle, topbarGhostButtonStyle, topbarPrimaryButtonStyle } from './styles'
import type { ActivityItem, PreviewState, Project, Session, Task, TaskView, View } from './types'
import { ActivityView, HomeView, PreviewPanel, ProjectsView, SessionsView, TasksWorkspace } from './views'

export default function App() {
	const [activeView, setActiveView] = useState<View>('tasks')
	const [taskView, setTaskView] = useState<TaskView>('list')
	const [selectedProject, setSelectedProject] = useState<string>('')
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
	const [searchQuery, setSearchQuery] = useState('')
	const [sessionMode, setSessionMode] = useState<'upload' | 'register' | null>(null)
	const [sessionTitle, setSessionTitle] = useState('')
	const [sessionDescription, setSessionDescription] = useState('')
	const [sessionArtifactPath, setSessionArtifactPath] = useState('')
	const [sessionArtifactName, setSessionArtifactName] = useState('')
	const [sessionFile, setSessionFile] = useState<File | null>(null)
	const [taskDraftTitle, setTaskDraftTitle] = useState('')
	const [taskDraftDescription, setTaskDraftDescription] = useState('')
	const [taskDraftPriority, setTaskDraftPriority] = useState<Task['priority']>('P1')
	const [taskDraftLabels, setTaskDraftLabels] = useState('')

	const [projects, setProjects] = useState<Project[]>([])
	const [tasks, setTasks] = useState<Task[]>([])
	const [taskDetails, setTaskDetails] = useState<Record<string, Task>>({})
	const [sessions, setSessions] = useState<Session[]>([])
	const [activities, setActivities] = useState<ActivityItem[]>([])
	const [homeProject, setHomeProject] = useState<Project | null>(null)
	const [homeTasks, setHomeTasks] = useState<Task[]>([])
	const [homeSessions, setHomeSessions] = useState<Session[]>([])
	const [homeActivities, setHomeActivities] = useState<ActivityItem[]>([])
	const [homeLoading, setHomeLoading] = useState(false)
	const [homeError, setHomeError] = useState<string>('')
	const sessionFileInputRef = useRef<HTMLInputElement | null>(null)
	const projectBySlug = useMemo(() => new Map(projects.map((project) => [project.slug, project])), [projects])
	const projectByID = useMemo(() => new Map(projects.map((project) => [project.id, project])), [projects])

	async function refreshSessions() {
		const items = await loadSessions()
		setSessions(items)
	}

	function resetSessionForm() {
		setSessionMode(null)
		setSessionTitle('')
		setSessionDescription('')
		setSessionArtifactPath('')
		setSessionArtifactName('')
		setSessionFile(null)
		if (sessionFileInputRef.current) {
			sessionFileInputRef.current.value = ''
		}
	}

	async function handleSubmitSession() {
		try {
			if (!sessionTitle.trim()) {
				window.alert('Session title is required')
				return
			}
			let artifactName = sessionArtifactName.trim()
			let artifactPath = sessionArtifactPath.trim()
			if (sessionMode === 'upload') {
				if (!sessionFile) {
					window.alert('Select an artifact file first')
					return
				}
				const uploaded = await uploadSessionArtifact(sessionFile)
				artifactName = uploaded.artifactName
				artifactPath = uploaded.artifactPath
			}
			if (!artifactName || !artifactPath) {
				window.alert('Artifact name and path are required')
				return
			}
			const session = await createSession({ title: sessionTitle.trim(), description: sessionDescription.trim(), artifactName, artifactPath })
			await refreshSessions()
			setPreview({ kind: 'session', snapshotId: session.snapshot_id })
			setActiveView('sessions')
			resetSessionForm()
		} catch (error) {
			window.alert(error instanceof Error ? error.message : 'Session save failed')
		}
	}

	async function handleCreateTask() {
		if (!activeProject?.id) return
		if (!taskDraftTitle.trim()) {
			window.alert('Task title is required')
			return
		}
		await createTask(activeProject.id, {
			title: taskDraftTitle.trim(),
			description: taskDraftDescription.trim(),
			priority: taskDraftPriority,
			labels: taskDraftLabels.split(',').map((label) => label.trim()).filter(Boolean),
		})
		setTaskDraftTitle('')
		setTaskDraftDescription('')
		setTaskDraftPriority('P1')
		setTaskDraftLabels('')
		loadTasks(activeProject.id, activeProject.slug, activeProject.name).then(setTasks).catch(() => setTasks([]))
		setActiveView('tasks')
	}

	async function handleUpdateTask(input: { taskId: string; title: string; description: string; status: Task['status']; priority: Task['priority']; assignee: string; labels: string[] }) {
		const updated = await updateTask(input.taskId, input)
		const project = projectByID.get(updated.project_id) ?? activeProject
		const mappedTask: Task = {
			id: updated.id,
			title: updated.title,
			project: project?.slug ?? updated.project_id,
			projectName: project?.name ?? updated.project_id,
			status: updated.status as Task['status'],
			priority: updated.priority as Task['priority'],
			assignee: updated.assignee_id ?? 'unassigned',
			labels: updated.labels ?? [],
			summary: updated.description,
			updatedAt: updated.updated_at,
			parent: updated.parent_task_id ?? undefined,
		}
		setTasks((current) => current.map((task) => (task.id === mappedTask.id ? { ...task, ...mappedTask } : task)))
		setTaskDetails((current) => {
			const existing = current[mappedTask.id]
			return {
				...current,
				[mappedTask.id]: existing ? { ...existing, ...mappedTask } : mappedTask,
			}
		})
	}

	useEffect(() => {
		loadProjects().then(setProjects).catch(() => setProjects([]))
		refreshSessions().catch(() => setSessions([]))
		loadActivities('').then(setActivities).catch(() => setActivities([]))
	}, [])

	useEffect(() => {
		if (selectedProject || projects.length === 0) return
		setSelectedProject(projects[0].slug)
		setTaskFilterProject(projects[0].slug)
	}, [projects, selectedProject])

	useEffect(() => {
		const params = new URLSearchParams()
		if (activityProject) params.set('project', activityProject)
		if (activityTask) params.set('task', activityTask)
		if (activityLabel) params.set('label', activityLabel)
		if (notProject) params.set('not_project', 'true')
		if (notTask) params.set('not_task', 'true')
		if (notLabel) params.set('not_label', 'true')
		params.set('sort', activitySort)
		const query = params.size > 0 ? `?${params.toString()}` : ''
		loadActivities(query).then(setActivities).catch(() => setActivities([]))
	}, [activityProject, activityTask, activityLabel, notProject, notTask, notLabel, activitySort])

	const activeProject = projects.find((project) => project.slug === selectedProject) ?? projects[0] ?? null

	useEffect(() => {
		setHomeError('')
	}, [activeProject?.id])

	useEffect(() => {
		if (!activeProject?.id) return
		setHomeLoading(true)
		setHomeError('')
		loadTasks(activeProject.id, activeProject.slug, activeProject.name).then(setTasks).catch(() => setTasks([]))
		loadProjectOverview(activeProject.id)
			.then((overview) => {
				setHomeProject(overview.project)
				setHomeTasks(overview.recentTasks)
				setHomeSessions(overview.recentSessions)
				setHomeActivities(overview.recentActivities)
				setProjects((current) => current.map((project) => (project.id === overview.project.id ? overview.project : project)))
				setHomeLoading(false)
			})
			.catch(() => {
				setHomeProject(activeProject)
				setHomeTasks([])
				setHomeSessions([])
				setHomeActivities([])
				setHomeError('Failed to load project overview.')
				setHomeLoading(false)
			})
	}, [activeProject?.id])

	useEffect(() => {
		if (!selectedTaskId) return
		const taskProject = tasks.find((task) => task.id === selectedTaskId)
		const detailProject = (taskProject?.project && projectBySlug.get(taskProject.project)) ?? activeProject
		if (!detailProject) return
		loadTaskDetail(selectedTaskId, detailProject.slug, detailProject.name)
			.then((task) => setTaskDetails((current) => ({ ...current, [task.id]: task })))
			.catch(() => undefined)
	}, [selectedTaskId, activeProject, tasks, projectBySlug])

	const selectedTask = taskDetails[selectedTaskId] ?? tasks.find((task) => task.id === selectedTaskId) ?? tasks[0] ?? null

	const filteredTasks = useMemo(() => {
		if (searchQuery.trim()) {
			return tasks
		}
		return tasks.filter((task) => {
			if (taskFilterProject && task.project !== taskFilterProject) return false
			if (taskView === 'mine' && task.assignee !== 'builder-agent' && task.assignee !== 'ui-agent') return false
			if (taskFilterLabel && !task.labels.includes(taskFilterLabel)) return false
			return true
		})
	}, [searchQuery, taskFilterLabel, taskFilterProject, taskView, tasks])

	const filteredActivities = useMemo(() => activities, [activities])

	const taskProjects = Array.from(new Set(projects.map((project) => project.slug)))
	const taskLabels = Array.from(new Set(tasks.flatMap((task) => task.labels ?? [])))
	const activityProjects = Array.from(new Set(activities.map((item) => item.project).filter(Boolean)))
	const activityTasks = Array.from(new Set(activities.map((item) => item.task)))
	const activityLabels = Array.from(new Set(activities.flatMap((item) => item.labels ?? [])))

	function mapSearchedTasks(items: Task[]) {
		return items.map((task) => {
			const project = projectByID.get(task.project) ?? projectBySlug.get(task.project)
			if (!project) return task
			return {
				...task,
				project: project.slug,
				projectName: project.name,
			}
		})
	}

	const activeTaskFilters = [taskFilterProject ? `project:${taskFilterProject}` : '', taskFilterLabel ? `label:${taskFilterLabel}` : ''].filter(Boolean)
	const activeActivityFilters = [
		activityProject ? `${notProject ? 'NOT ' : ''}project:${activityProject}` : '',
		activityTask ? `${notTask ? 'NOT ' : ''}task:${activityTask}` : '',
		activityLabel ? `${notLabel ? 'NOT ' : ''}label:${activityLabel}` : '',
	].filter(Boolean)

	return (
		<div style={{ minHeight: '100vh', background: palette.app, color: palette.text, fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif' }}>
			<div style={{ display: 'grid', gridTemplateColumns: '260px minmax(0, 1fr)', minHeight: '100vh' }}>
				<aside style={{ background: palette.sidebar, color: '#edf4ff', padding: 18, borderRight: '1px solid rgba(255,255,255,0.05)', display: 'flex', flexDirection: 'column', gap: 18 }}>
					<div style={{ display: 'grid', gap: 10 }}>
						<div style={{ color: '#7fd0ff', fontSize: 12, textTransform: 'uppercase', letterSpacing: 1.4 }}>Agent Task Manager</div>
						<div style={{ fontSize: 22, fontWeight: 800, lineHeight: 1.1 }}>Operational work shell for tasks, audit, and agent snapshots</div>
						<div style={{ color: '#91a0b8', fontSize: 13, lineHeight: 1.6 }}>Focused application shell with one stable navigation model and context-preserving detail flows.</div>
					</div>
					<nav style={{ display: 'grid', gap: 6 }}>
						{navItems.map((item) => {
							const active = activeView === item.id
							return (
								<button key={item.id} onClick={() => setActiveView(item.id)} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '11px 12px', borderRadius: 12, border: active ? '1px solid rgba(127,208,255,0.45)' : '1px solid transparent', background: active ? 'rgba(28,155,255,0.16)' : 'transparent', color: active ? '#f6fbff' : '#b5c1d3', fontWeight: active ? 700 : 600, cursor: 'pointer' }}>
									<div style={{ width: 28, height: 28, borderRadius: 9, background: active ? 'rgba(127,208,255,0.14)' : 'rgba(255,255,255,0.06)', display: 'grid', placeItems: 'center', fontSize: 11, letterSpacing: 0.3 }}>{item.short}</div>
									<span>{item.label}</span>
								</button>
							)
						})}
					</nav>
					<div style={{ marginTop: 'auto', display: 'grid', gap: 12 }}>
						<div style={sidebarInfoCardStyle}><div style={sidebarLabelStyle}>Current Scope</div><div style={{ fontWeight: 700 }}>{activeProject?.name ?? 'Loading'}</div><div style={{ color: '#91a0b8', fontSize: 13 }}>{activeProject?.slug ?? selectedProject}</div></div>
						<div style={sidebarInfoCardStyle}><div style={sidebarLabelStyle}>Operator</div><div style={{ fontWeight: 700 }}>admin@atm.dev</div><div style={{ color: '#91a0b8', fontSize: 13 }}>API auth and UI session states should surface explicitly.</div></div>
					</div>
				</aside>
				<div style={{ background: palette.workspace, minWidth: 0 }}>
					<header style={{ height: 72, padding: '0 22px', background: palette.topbar, borderBottom: '1px solid rgba(255,255,255,0.06)', display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) auto auto', gap: 16, alignItems: 'center' }}>
						<input value={searchQuery} onChange={(event) => setSearchQuery(event.target.value)} onKeyDown={(event) => {
							if (event.key !== 'Enter') return
							const value = event.currentTarget.value.trim()
							if (!value) {
								if (activeProject?.id) loadTasks(activeProject.id, activeProject.slug, activeProject.name).then(setTasks).catch(() => setTasks([]))
								return
							}
							searchTasks(value).then((items) => setTasks(mapSearchedTasks(items))).catch(() => setTasks([]))
						}} style={{ background: 'rgba(255,255,255,0.06)', border: '1px solid rgba(255,255,255,0.08)', borderRadius: 12, padding: '11px 14px', color: '#d5deea', fontSize: 14 }} placeholder="Search tasks, comments, sessions, and activity" />
						<div style={{ color: '#d3deeb', fontSize: 13 }}>Scope: {activeProject?.slug ?? selectedProject}</div>
						<div style={{ display: 'flex', gap: 10 }}><button style={topbarGhostButtonStyle}>Command</button><button style={topbarPrimaryButtonStyle} onClick={() => setActiveView('tasks')}>Quick Create</button></div>
					</header>
					<div style={{ padding: 20 }}>
						{activeView === 'home' && (homeProject ?? activeProject) && <HomeView project={homeProject ?? activeProject!} tasks={homeTasks} activities={homeActivities} sessions={homeSessions} onOpenTask={(taskId) => { setSelectedTaskId(taskId); setPreview({ kind: 'task', taskId }); setActiveView('tasks') }} loading={homeLoading} error={homeError} />}
						{activeView === 'projects' && <ProjectsView projects={projects} selectedProject={selectedProject} onSelectProject={(slug) => { setSelectedProject(slug); setTaskFilterProject(slug); setActiveView('tasks') }} />}
						{activeView === 'tasks' && selectedTask && <TasksWorkspace taskView={taskView} onChangeTaskView={setTaskView} selectedProject={taskFilterProject} onSelectProject={setTaskFilterProject} selectedLabel={taskFilterLabel} onSelectLabel={setTaskFilterLabel} projects={taskProjects} labels={taskLabels} filters={activeTaskFilters} tasks={filteredTasks} selectedTask={selectedTask} onSelectTask={(taskId) => { setSelectedTaskId(taskId); setPreview({ kind: 'task', taskId }) }} onClearFilters={() => { setTaskFilterProject(selectedProject); setTaskFilterLabel('') }} onCreateTask={handleCreateTask} taskDraftTitle={taskDraftTitle} onTaskDraftTitleChange={setTaskDraftTitle} taskDraftDescription={taskDraftDescription} onTaskDraftDescriptionChange={setTaskDraftDescription} taskDraftPriority={taskDraftPriority} onTaskDraftPriorityChange={setTaskDraftPriority} taskDraftLabels={taskDraftLabels} onTaskDraftLabelsChange={setTaskDraftLabels} onUpdateTask={(input) => { void handleUpdateTask(input) }} />}
						{activeView === 'sessions' && <SessionsView sessions={sessions} onPreview={(snapshotId) => setPreview({ kind: 'session', snapshotId })} onDownload={(snapshotId) => window.open(sessionDownloadURL(snapshotId), '_blank', 'noopener,noreferrer')} sessionMode={sessionMode} onSessionModeChange={setSessionMode} sessionTitle={sessionTitle} onSessionTitleChange={setSessionTitle} sessionDescription={sessionDescription} onSessionDescriptionChange={setSessionDescription} sessionArtifactName={sessionArtifactName} onSessionArtifactNameChange={setSessionArtifactName} sessionArtifactPath={sessionArtifactPath} onSessionArtifactPathChange={setSessionArtifactPath} sessionFile={sessionFile} onSessionFileChange={setSessionFile} onSubmitSession={() => { void handleSubmitSession() }} onCancelSession={resetSessionForm} fileInputRef={sessionFileInputRef} />}
		{activeView === 'activity' && <ActivityView activities={activities} projects={activityProjects} tasks={activityTasks} labels={activityLabels} project={activityProject} task={activityTask} label={activityLabel} notProject={notProject} notTask={notTask} notLabel={notLabel} sort={activitySort} onProjectChange={setActivityProject} onTaskChange={setActivityTask} onLabelChange={setActivityLabel} onToggleNotProject={() => setNotProject((value) => !value)} onToggleNotTask={() => setNotTask((value) => !value)} onToggleNotLabel={() => setNotLabel((value) => !value)} onToggleSort={() => setActivitySort((value) => (value === 'asc' ? 'desc' : 'asc'))} onPreview={(activityId) => setPreview({ kind: 'activity', activityId })} onClear={() => { setActivityProject(''); setActivityTask(''); setActivityLabel(''); setNotProject(false); setNotTask(false); setNotLabel(false) }} activeFilters={activeActivityFilters} />}
					</div>
				</div>
			</div>
			{preview && <PreviewPanel preview={preview} tasks={tasks} activities={activities} sessions={sessions} onClose={() => setPreview(null)} onOpenTaskDetail={(taskId) => { setSelectedTaskId(taskId); setActiveView('tasks') }} onDownloadSession={(snapshotId) => window.open(sessionDownloadURL(snapshotId), '_blank', 'noopener,noreferrer')} />}
		</div>
	)
}
