import { useMemo, useState } from 'react'

import { activities, projects, sessions, tasks } from './mock-data'
import { navItems } from './ui-config'
import { palette, sidebarInfoCardStyle, sidebarLabelStyle, topbarGhostButtonStyle, topbarPrimaryButtonStyle } from './styles'
import type { PreviewState, TaskView, View } from './types'
import { ActivityView, HomeView, PreviewPanel, ProjectsView, SessionsView, TasksWorkspace } from './views'

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
			if (taskFilterProject && task.project !== taskFilterProject) return false
			if (taskView === 'mine' && task.assignee !== 'builder-agent' && task.assignee !== 'ui-agent') return false
			if (taskFilterLabel && !task.labels.includes(taskFilterLabel)) return false
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
						<div style={sidebarInfoCardStyle}><div style={sidebarLabelStyle}>Current Scope</div><div style={{ fontWeight: 700 }}>{activeProject.name}</div><div style={{ color: '#91a0b8', fontSize: 13 }}>{activeProject.slug}</div></div>
						<div style={sidebarInfoCardStyle}><div style={sidebarLabelStyle}>Operator</div><div style={{ fontWeight: 700 }}>admin@atm.dev</div><div style={{ color: '#91a0b8', fontSize: 13 }}>API auth and UI session states should surface explicitly.</div></div>
					</div>
				</aside>
				<div style={{ background: palette.workspace, minWidth: 0 }}>
					<header style={{ height: 72, padding: '0 22px', background: palette.topbar, borderBottom: '1px solid rgba(255,255,255,0.06)', display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) auto auto', gap: 16, alignItems: 'center' }}>
						<div style={{ background: 'rgba(255,255,255,0.06)', border: '1px solid rgba(255,255,255,0.08)', borderRadius: 12, padding: '11px 14px', color: '#d5deea', fontSize: 14 }}>Search tasks, comments, sessions, and activity</div>
						<div style={{ color: '#d3deeb', fontSize: 13 }}>Scope: {activeProject.slug}</div>
						<div style={{ display: 'flex', gap: 10 }}><button style={topbarGhostButtonStyle}>Command</button><button style={topbarPrimaryButtonStyle}>Quick Create</button></div>
					</header>
					<div style={{ padding: 20 }}>
						{activeView === 'home' && <HomeView project={activeProject} tasks={filteredTasks} activities={filteredActivities} sessions={sessions} onOpenTask={(taskId) => { setSelectedTaskId(taskId); setPreview({ kind: 'task', taskId }); setActiveView('tasks') }} />}
						{activeView === 'projects' && <ProjectsView projects={projects} selectedProject={selectedProject} onSelectProject={(slug) => { setSelectedProject(slug); setTaskFilterProject(slug); setActiveView('tasks') }} />}
						{activeView === 'tasks' && <TasksWorkspace taskView={taskView} onChangeTaskView={setTaskView} selectedProject={taskFilterProject} onSelectProject={setTaskFilterProject} selectedLabel={taskFilterLabel} onSelectLabel={setTaskFilterLabel} projects={taskProjects} labels={taskLabels} filters={activeTaskFilters} tasks={filteredTasks} selectedTask={selectedTask} onSelectTask={(taskId) => { setSelectedTaskId(taskId); setPreview({ kind: 'task', taskId }) }} onClearFilters={() => { setTaskFilterProject(selectedProject); setTaskFilterLabel('') }} />}
						{activeView === 'sessions' && <SessionsView sessions={sessions} onPreview={(snapshotId) => setPreview({ kind: 'session', snapshotId })} />}
						{activeView === 'activity' && <ActivityView activities={filteredActivities} projects={activityProjects} tasks={activityTasks} labels={activityLabels} project={activityProject} task={activityTask} label={activityLabel} notProject={notProject} notTask={notTask} notLabel={notLabel} sort={activitySort} onProjectChange={setActivityProject} onTaskChange={setActivityTask} onLabelChange={setActivityLabel} onToggleNotProject={() => setNotProject((value) => !value)} onToggleNotTask={() => setNotTask((value) => !value)} onToggleNotLabel={() => setNotLabel((value) => !value)} onToggleSort={() => setActivitySort((value) => (value === 'asc' ? 'desc' : 'asc'))} onPreview={(activityId) => setPreview({ kind: 'activity', activityId })} onClear={() => { setActivityProject(''); setActivityTask(''); setActivityLabel(''); setNotProject(false); setNotTask(false); setNotLabel(false) }} activeFilters={activeActivityFilters} />}
					</div>
				</div>
			</div>
			{preview && <PreviewPanel preview={preview} tasks={tasks} activities={activities} sessions={sessions} onClose={() => setPreview(null)} onOpenTaskDetail={(taskId) => { setSelectedTaskId(taskId); setActiveView('tasks') }} />}
		</div>
	)
}
