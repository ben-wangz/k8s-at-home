import { useEffect, useMemo, useState } from 'react'

import { loadActivities, loadProjectOverview, loadProjects, loadSessions, loadTaskDetail, loadTasks, sessionDownloadURL } from './api'
import { navItems } from './ui-config'
import { palette, sidebarInfoCardStyle, sidebarLabelStyle, topbarGhostButtonStyle } from './styles'
import type { ActivityItem, PreviewState, Project, Session, Task, TaskView, View } from './types'
import { ActivityView, HomeView, PreviewPanel, ProjectsView, SessionsView, TaskDetailPage, TasksWorkspace } from './views'

export default function App() {
	const [activeView, setActiveView] = useState<View>('tasks')
	const [taskView, setTaskView] = useState<TaskView>('board')
	const [selectedProject, setSelectedProject] = useState<string>('')
	const [selectedTaskId, setSelectedTaskId] = useState<string>('')
	const [preview, setPreview] = useState<PreviewState>(null)
	const [taskFilterProject, setTaskFilterProject] = useState<string>('agent-task-manager')
	const [taskFilterLabel, setTaskFilterLabel] = useState<string>('')
	const [activityProject, setActivityProject] = useState<string>('')
	const [activityTask, setActivityTask] = useState<string>('')
	const [activityLabel, setActivityLabel] = useState<string>('')
	const [notProject, setNotProject] = useState(false)
	const [notTask, setNotTask] = useState(false)
	const [notLabel, setNotLabel] = useState(false)
	const [activitySort, setActivitySort] = useState<'asc' | 'desc'>('desc')
	const [projectSearchDraft, setProjectSearchDraft] = useState('')
	const [projectSearch, setProjectSearch] = useState('')
	const [projectsRefreshKey, setProjectsRefreshKey] = useState(0)
	const [sessionsRefreshKey, setSessionsRefreshKey] = useState(0)
	const [activitiesRefreshKey, setActivitiesRefreshKey] = useState(0)
	const [workspaceRefreshKey, setWorkspaceRefreshKey] = useState(0)
	const [taskDetailRefreshKey, setTaskDetailRefreshKey] = useState(0)

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
	const projectBySlug = useMemo(() => new Map(projects.map((project) => [project.slug, project])), [projects])
	const projectByID = useMemo(() => new Map(projects.map((project) => [project.id, project])), [projects])

	async function refreshSessions() {
		const items = await loadSessions()
		setSessions(items)
	}

	function refreshCurrentView() {
		switch (activeView) {
			case 'projects':
				setProjectsRefreshKey((value) => value + 1)
				break
			case 'sessions':
				setSessionsRefreshKey((value) => value + 1)
				break
			case 'activity':
				setActivitiesRefreshKey((value) => value + 1)
				break
			case 'task-detail':
				setWorkspaceRefreshKey((value) => value + 1)
				setTaskDetailRefreshKey((value) => value + 1)
				break
			default:
				setWorkspaceRefreshKey((value) => value + 1)
		}
	}

	useEffect(() => {
		loadProjects().then(setProjects).catch(() => setProjects([]))
	}, [projectsRefreshKey])

	useEffect(() => {
		refreshSessions().catch(() => setSessions([]))
	}, [sessionsRefreshKey])

	useEffect(() => {
		loadActivities('').then(setActivities).catch(() => setActivities([]))
	}, [])

	useEffect(() => {
		if (selectedProject || projects.length === 0) return
		setSelectedProject(projects[0].slug)
		setTaskFilterProject(projects[0].slug)
		setSelectedTaskId('')
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
	}, [activitiesRefreshKey, activityProject, activityTask, activityLabel, notProject, notTask, notLabel, activitySort])

	const activeProject = projects.find((project) => project.slug === selectedProject) ?? projects[0] ?? null

	useEffect(() => {
		setHomeError('')
	}, [activeProject?.id, workspaceRefreshKey])

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
	}, [activeProject?.id, workspaceRefreshKey])

	useEffect(() => {
		if (!selectedTaskId) return
		const taskProject = tasks.find((task) => task.id === selectedTaskId)
		const detailProject = (taskProject?.project && projectBySlug.get(taskProject.project)) ?? activeProject
		if (!detailProject) return
		loadTaskDetail(selectedTaskId, detailProject.slug, detailProject.name)
			.then((task) => setTaskDetails((current) => ({ ...current, [task.id]: task })))
			.catch(() => undefined)
	}, [selectedTaskId, activeProject, taskDetailRefreshKey, tasks, projectBySlug])

	const selectedTask = taskDetails[selectedTaskId] ?? tasks.find((task) => task.id === selectedTaskId) ?? null

	const filteredTasks = useMemo(() => {
		return tasks.filter((task) => {
			if (taskFilterProject && task.project !== taskFilterProject) return false
			if (taskFilterLabel && !task.labels.includes(taskFilterLabel)) return false
			return true
		})
	}, [taskFilterLabel, taskFilterProject, tasks])

	const taskProjects = projects.map((project) => ({ slug: project.slug, name: project.name }))
	const taskLabels = Array.from(new Set(tasks.flatMap((task) => task.labels ?? [])))
	const activityProjects = Array.from(new Map(activities.filter((item) => item.project).map((item) => [item.project, { id: item.project, name: item.projectName || item.project }])).values())
	const activityTasks = Array.from(new Map(activities.filter((item) => item.task).map((item) => [item.task, { id: item.task, name: tasks.find((task) => task.id === item.task)?.title ?? item.task }])).values())
	const activityLabels = Array.from(new Set(activities.flatMap((item) => item.labels ?? [])))
	const filteredProjects = useMemo(() => {
		const query = projectSearch.trim().toLowerCase()
		const sorted = [...projects].sort((left, right) => left.name.localeCompare(right.name))
		if (!query) return sorted
		return sorted.filter((project) => project.name.toLowerCase().includes(query))
	}, [projectSearch, projects])

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
					<header style={{ height: 72, padding: '0 22px', background: palette.topbar, borderBottom: '1px solid rgba(255,255,255,0.06)', display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) auto', gap: 16, alignItems: 'center' }}>
						<div style={{ color: '#d3deeb', fontSize: 13 }}>Agent-first workspace</div>
						<div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
							<div style={{ color: '#d3deeb', fontSize: 13 }}>Scope: {activeProject?.slug ?? selectedProject}</div>
							<button style={topbarGhostButtonStyle} onClick={refreshCurrentView}>Refresh</button>
						</div>
					</header>
					<div style={{ padding: 20 }}>
						{activeView === 'home' && (homeProject ?? activeProject) && <HomeView project={homeProject ?? activeProject!} tasks={homeTasks} activities={homeActivities} sessions={homeSessions} onOpenTask={(taskId) => { setSelectedTaskId(taskId); setActiveView('task-detail') }} loading={homeLoading} error={homeError} />}
						{activeView === 'projects' && <ProjectsView projects={filteredProjects} selectedProject={selectedProject} searchValue={projectSearchDraft} onSearchValueChange={setProjectSearchDraft} onSearch={() => setProjectSearch(projectSearchDraft)} onSelectProject={(slug) => { setSelectedProject(slug); setTaskFilterProject(slug); setActiveView('tasks') }} />}
						{activeView === 'tasks' && <TasksWorkspace taskView={taskView} onChangeTaskView={setTaskView} selectedProject={taskFilterProject} onSelectProject={setTaskFilterProject} selectedLabel={taskFilterLabel} onSelectLabel={setTaskFilterLabel} projects={taskProjects} labels={taskLabels} filters={activeTaskFilters} tasks={filteredTasks} selectedTaskId={selectedTaskId} onSelectTask={(taskId) => { setSelectedTaskId(taskId); setActiveView('task-detail') }} onClearFilters={() => { setTaskFilterProject(selectedProject); setTaskFilterLabel('') }} />}
						{activeView === 'task-detail' && selectedTask && <TaskDetailPage task={selectedTask} onBack={() => setActiveView('tasks')} />}
						{activeView === 'sessions' && <SessionsView sessions={sessions} onPreview={(snapshotId) => setPreview({ kind: 'session', snapshotId })} onDownload={(snapshotId) => window.open(sessionDownloadURL(snapshotId), '_blank', 'noopener,noreferrer')} />}
						{activeView === 'activity' && <ActivityView activities={activities} projects={activityProjects} tasks={activityTasks} labels={activityLabels} project={activityProject} task={activityTask} label={activityLabel} notProject={notProject} notTask={notTask} notLabel={notLabel} sort={activitySort} onProjectChange={setActivityProject} onTaskChange={setActivityTask} onLabelChange={setActivityLabel} onToggleNotProject={() => setNotProject((value) => !value)} onToggleNotTask={() => setNotTask((value) => !value)} onToggleNotLabel={() => setNotLabel((value) => !value)} onToggleSort={() => setActivitySort((value) => (value === 'asc' ? 'desc' : 'asc'))} onPreview={(activityId) => setPreview({ kind: 'activity', activityId })} onClear={() => { setActivityProject(''); setActivityTask(''); setActivityLabel(''); setNotProject(false); setNotTask(false); setNotLabel(false) }} activeFilters={activeActivityFilters} />}
					</div>
				</div>
			</div>
			{preview && <PreviewPanel preview={preview} tasks={tasks} activities={activities} sessions={sessions} onClose={() => setPreview(null)} onOpenTaskDetail={(taskId) => { setSelectedTaskId(taskId); setActiveView('task-detail') }} onDownloadSession={(snapshotId) => window.open(sessionDownloadURL(snapshotId), '_blank', 'noopener,noreferrer')} />}
		</div>
	)
}
