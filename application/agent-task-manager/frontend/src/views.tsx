import type { CSSProperties } from 'react'

import { priorityMeta, statusMeta, taskViewItems } from './ui-config'
import {
	activityNoteStyle,
	boardCardStyle,
	boardColumnHeaderStyle,
	boardColumnStyle,
	clearActionButtonStyle,
	closeButtonStyle,
	commentRowStyle,
	detailBlockStyle,
	detailBlockTitleStyle,
	eyebrowStyle,
	ghostActionButtonStyle,
	inlineActionButtonStyle,
	inlineGhostActionButtonStyle,
	listButtonStyle,
	metaLabelStyle,
	metaTextStyle,
	palette,
	previewHeaderStyle,
	primaryActionButtonStyle,
	registryCardStyle,
	scopePillStyle,
	segmentedButtonStyle,
	selectStyle,
	subtaskRowStyle,
	surfaceStyle,
	tableCellStyle,
	tableHeadStyle,
	tableStyle,
	timelineButtonStyle,
	timelineDotStyle,
	timelineRowStyle,
	toolbarStyle,
	toolbarTitleStyle,
} from './styles'
import type { ActivityItem, PreviewState, Priority, Project, Session, Task, TaskStatus, TaskView } from './types'

export function HomeView({
	project,
	tasks,
	activities,
	sessions,
	onOpenTask,
	loading = false,
	error = '',
}: {
	project: Project
	tasks: Task[]
	activities: ActivityItem[]
	sessions: Session[]
	onOpenTask: (taskId: string) => void
	loading?: boolean
	error?: string
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
				{loading && <div style={{ color: palette.muted, fontSize: 14 }}>Loading project overview...</div>}
				{!loading && error && <div style={{ color: palette.warning, fontSize: 14 }}>{error}</div>}
			</div>
			<div style={{ display: 'grid', gridTemplateColumns: '1.3fr 1fr', gap: 18, alignItems: 'start' }}>
				<div style={{ display: 'grid', gap: 18 }}>
					<div style={surfaceStyle}>
						<SectionHeading title="My open tasks" subtitle="The first screen should answer what needs attention now." />
						<div style={{ display: 'grid', gap: 10 }}>
							{tasks.length === 0 && <div style={{ color: palette.muted }}>No recent tasks yet.</div>}
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
							{activities.length === 0 && <div style={{ color: palette.muted }}>No recent activity yet.</div>}
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
							{sessions.length === 0 && <div style={{ color: palette.muted }}>No recent sessions yet.</div>}
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

export function ProjectsView({
	projects,
	selectedProject,
	searchValue,
	onSearchValueChange,
	onSearch,
	onSelectProject,
}: {
	projects: Project[]
	selectedProject: string
	searchValue: string
	onSearchValueChange: (value: string) => void
	onSearch: () => void
	onSelectProject: (slug: string) => void
}) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader eyebrow="Projects" title="Dense project index with fast scope switching" description="Projects use a table-first pattern instead of oversized card walls. Selecting a project should move directly into task work." />
			<div style={surfaceStyle}>
				<div style={toolbarStyle}>
					<div style={toolbarTitleStyle}>Project Registry</div>
					<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
						<input value={searchValue} onChange={(event) => onSearchValueChange(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') onSearch() }} placeholder="Search project name" style={textInputStyle} />
						<button style={ghostActionButtonStyle} onClick={onSearch}>GO</button>
					</div>
				</div>
				<div style={{ overflowX: 'auto' }}>
					{projects.length === 0 && <EmptyState title="No projects match this search" detail="Refine the project name query or clear it to see all projects sorted by project name." />}
					{projects.length > 0 && (
					<table style={tableStyle}>
						<thead>
							<tr>
								{['Project', 'Slug', 'Open', 'In Progress', 'In Review', 'Updated', 'Actions'].map((header) => (
									<th key={header} style={tableHeadStyle}>{header}</th>
								))}
							</tr>
						</thead>
						<tbody>
							{projects.map((project) => {
								const active = project.slug === selectedProject
								return (
									<tr key={project.id} style={{ background: active ? palette.accentSoft : 'transparent' }}>
										<td style={tableCellStyle}><div style={{ fontWeight: 700 }}>{project.name}</div><div style={{ color: palette.muted, fontSize: 13, marginTop: 4 }}>{project.summary}</div></td>
										<td style={{ ...tableCellStyle, fontFamily: 'ui-monospace, SFMono-Regular, monospace' }}>{project.slug}</td>
										<td style={tableCellStyle}>{project.open}</td>
										<td style={tableCellStyle}>{project.inProgress}</td>
										<td style={tableCellStyle}>{project.inReview}</td>
										<td style={tableCellStyle}>{project.updatedAt}</td>
										<td style={tableCellStyle}><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}><button style={inlineActionButtonStyle} onClick={() => onSelectProject(project.slug)}>Open Tasks</button><button style={inlineGhostActionButtonStyle} onClick={() => navigator.clipboard.writeText(project.id)}>Copy ID</button></div></td>
									</tr>
								)
							})}
						</tbody>
					</table>
					)}
				</div>
			</div>
		</div>
	)
}

export function TasksWorkspace(props: {
	taskView: TaskView
	onChangeTaskView: (view: TaskView) => void
	selectedProject: string
	onSelectProject: (project: string) => void
	selectedLabel: string
	onSelectLabel: (label: string) => void
	projects: Array<{ slug: string; name: string }>
	labels: string[]
	filters: string[]
	tasks: Task[]
	selectedTaskId: string
	onSelectTask: (taskId: string) => void
	onClearFilters: () => void
}) {
	const { taskView, onChangeTaskView, selectedProject, onSelectProject, selectedLabel, onSelectLabel, projects, labels, filters, tasks, selectedTaskId, onSelectTask, onClearFilters } = props
	const grouped = taskView === 'board' ? {
		backlog: tasks.filter((task) => task.status === 'backlog'),
		todo: tasks.filter((task) => task.status === 'todo'),
		in_progress: tasks.filter((task) => task.status === 'in_progress'),
		in_review: tasks.filter((task) => task.status === 'in_review'),
	} : null

	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader eyebrow="Tasks" title="Workspace for list, board, and task-detail flows" description="One workspace with internal view switching. The board is now a mode inside Tasks, not a separate top-level product section." />
			<div style={surfaceStyle}>
				<div style={toolbarStyle}>
					<div style={{ display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap' }}>
						<div style={toolbarTitleStyle}>Tasks Workspace</div>
						<div style={scopePillStyle}>Scope: {selectedProject}</div>
					</div>
					<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
						{taskViewItems.map((item) => {
							const active = item.id === taskView
							return <button key={item.id} onClick={() => onChangeTaskView(item.id)} style={{ ...segmentedButtonStyle, background: active ? palette.text : palette.panel, color: active ? '#ffffff' : palette.text, borderColor: active ? palette.text : palette.border }}>{item.label}</button>
						})}
					</div>
				</div>
				<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 14 }}>
					<select value={selectedProject} onChange={(event) => onSelectProject(event.target.value)} style={selectStyle}>{projects.map((project) => <option key={project.slug} value={project.slug}>{project.name}</option>)}</select>
					<select value={selectedLabel} onChange={(event) => onSelectLabel(event.target.value)} style={selectStyle}><option value="">All labels</option>{labels.map((label) => <option key={label} value={label}>{label}</option>)}</select>
					<button style={ghostActionButtonStyle}>Filter rail</button>
					{filters.length > 0 && <><>{filters.map((filter) => <FilterChip key={filter} label={filter} />)}</><button style={clearActionButtonStyle} onClick={onClearFilters}>Clear filters</button></>}
				</div>
				{tasks.length === 0 ? <EmptyState title="No tasks match this scope" detail="The current project or label filters removed all visible tasks. Clear filters or switch scope to continue." /> : taskView === 'board' && grouped ? (
					<div style={{ overflowX: 'auto', paddingBottom: 4 }}>
						<div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 280px)', gap: 14, minWidth: 'max-content', alignItems: 'start' }}>
							{Object.entries(grouped).map(([key, columnTasks]) => <div key={key} style={boardColumnStyle}><div style={boardColumnHeaderStyle}><span>{statusMeta[key as TaskStatus].label}</span><span style={metaTextStyle}>{columnTasks.length}</span></div><div style={{ display: 'grid', gap: 10 }}>{columnTasks.map((task) => <button key={task.id} onClick={() => onSelectTask(task.id)} style={boardCardStyle}><div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, marginBottom: 8 }}><IdPill value={task.id} /><PriorityPill priority={task.priority} /></div><div style={{ fontWeight: 700, lineHeight: 1.35, marginBottom: 8, overflowWrap: 'anywhere' }}>{task.title}</div><div style={{ color: palette.muted, fontSize: 13, lineHeight: 1.5, marginBottom: 10, overflowWrap: 'anywhere' }}>{task.summary}</div><div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 8 }}>{task.labels.slice(0, 2).map((label) => <LabelChip key={label} label={label} />)}</div><div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, color: palette.subtle, fontSize: 12 }}><span style={{ overflowWrap: 'anywhere' }}>{task.assignee}</span><span>{task.updatedAt}</span></div></button>)}</div></div>)}
						</div>
					</div>
				) : (
					<div style={{ overflowX: 'auto' }}><table style={tableStyle}><thead><tr>{['ID', 'Title', 'Status', 'Priority', 'Labels', 'Assignee', 'Updated'].map((header) => <th key={header} style={tableHeadStyle}>{header}</th>)}</tr></thead><tbody>{tasks.map((task) => { const active = selectedTaskId === task.id; return <tr key={task.id} onClick={() => onSelectTask(task.id)} style={{ cursor: 'pointer', background: active ? palette.accentSoft : 'transparent' }}><td style={{ ...tableCellStyle, fontFamily: 'ui-monospace, SFMono-Regular, monospace' }}>{task.id}</td><td style={tableCellStyle}><div style={{ fontWeight: 700 }}>{task.title}</div><div style={{ color: palette.muted, fontSize: 13, marginTop: 4 }}>{task.summary}</div></td><td style={tableCellStyle}><StatusPill status={task.status} /></td><td style={tableCellStyle}><PriorityPill priority={task.priority} /></td><td style={tableCellStyle}><div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>{task.labels.map((label) => <LabelChip key={label} label={label} />)}</div></td><td style={tableCellStyle}>{task.assignee}</td><td style={tableCellStyle}>{task.updatedAt}</td></tr> })}</tbody></table></div>
				)}
			</div>
		</div>
	)
}

export function SessionsView(props: {
	sessions: Session[]
	onPreview: (snapshotId: string) => void
	onDownload: (snapshotId: string) => void
}) {
	const { sessions, onPreview, onDownload } = props
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader eyebrow="Sessions" title="Registry for imported agent session snapshots" description="A document-registry mental model: upload, register, inspect metadata, and download artifacts without pretending these are task cards." />
			<div style={surfaceStyle}>
				<div style={toolbarStyle}>
					<div style={toolbarTitleStyle}>Snapshot Registry</div>
				</div>
				<div style={{ overflowX: 'auto' }}>
					<table style={tableStyle}>
						<thead>
							<tr>
								{['Title', 'Snapshot ID', 'Description', 'Artifact Name', 'Artifact Path', 'Updated', 'Actions'].map((header) => <th key={header} style={tableHeadStyle}>{header}</th>)}
							</tr>
						</thead>
						<tbody>
							{sessions.map((session) => (
								<tr key={session.snapshotId}>
									<td style={tableCellStyle}><div style={{ fontWeight: 700 }}>{session.title}</div></td>
									<td style={{ ...tableCellStyle, fontFamily: 'ui-monospace, SFMono-Regular, monospace', fontSize: 12 }}>{session.snapshotId}</td>
									<td style={tableCellStyle}>{session.description}</td>
									<td style={tableCellStyle}>{session.artifactName}</td>
									<td style={{ ...tableCellStyle, maxWidth: 280 }}><div style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', fontFamily: 'ui-monospace, SFMono-Regular, monospace', fontSize: 12 }}>{session.artifactPath}</div></td>
									<td style={tableCellStyle}>{session.updatedAt}</td>
									<td style={tableCellStyle}><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}><button style={inlineActionButtonStyle} onClick={() => onPreview(session.snapshotId)}>Preview</button><button style={inlineGhostActionButtonStyle} onClick={() => onDownload(session.snapshotId)}>Download</button></div></td>
								</tr>
							))}
						</tbody>
					</table>
				</div>
			</div>
		</div>
	)
}

export function ActivityView(props: {
	activities: ActivityItem[]
	projects: Array<{ id: string; name: string }>
	tasks: Array<{ id: string; name: string }>
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
	const { activities, projects, tasks, labels, project, task, label, notProject, notTask, notLabel, sort, onProjectChange, onTaskChange, onLabelChange, onToggleNotProject, onToggleNotTask, onToggleNotLabel, onToggleSort, onPreview, onClear, activeFilters } = props
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader eyebrow="Activity" title="Global audit timeline with explicit combined filters" description="Project, task, and label filters share one visible filter model with removable chips, NOT state, and explicit sort direction." />
			<div style={surfaceStyle}>
				<div style={toolbarStyle}><div style={toolbarTitleStyle}>Activity Timeline</div><button style={ghostActionButtonStyle} onClick={onToggleSort}>Sort: {sort === 'asc' ? 'Ascending' : 'Descending'}</button></div>
				<div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
					<select value={project} onChange={(event) => onProjectChange(event.target.value)} style={selectStyle}><option value="">Project</option>{projects.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select>
					<ToggleChip label="NOT" active={notProject} onClick={onToggleNotProject} />
					<select value={task} onChange={(event) => onTaskChange(event.target.value)} style={selectStyle}><option value="">Task</option>{tasks.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select>
					<ToggleChip label="NOT" active={notTask} onClick={onToggleNotTask} />
					<select value={label} onChange={(event) => onLabelChange(event.target.value)} style={selectStyle}><option value="">Label</option>{labels.map((item) => <option key={item} value={item}>{item}</option>)}</select>
					<ToggleChip label="NOT" active={notLabel} onClick={onToggleNotLabel} />
					{activeFilters.map((filter) => <FilterChip key={filter} label={filter} />)}
					{activeFilters.length > 0 && <button style={clearActionButtonStyle} onClick={onClear}>Clear all</button>}
				</div>
				{activities.length === 0 ? <EmptyState title="No activity matches this filter set" detail="The current combined filter state removed all timeline rows. Clear filters or invert one condition to continue." /> : <div style={{ display: 'grid', gap: 12 }}>{activities.map((activity) => <button key={activity.id} onClick={() => onPreview(activity.id)} style={timelineButtonStyle}><div style={timelineDotStyle} /><div style={{ minWidth: 0 }}><div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, marginBottom: 4 }}><div style={{ fontWeight: 700 }}>{activity.title}</div><div style={metaTextStyle}>{activity.updatedAt}</div></div><div style={{ color: palette.muted, lineHeight: 1.55, marginBottom: 8 }}>{activity.detail}</div><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}><MetaBadge label={activity.projectName || 'No project'} /><MetaBadge label={activity.task || 'No task'} />{activity.labels.map((item) => <LabelChip key={item} label={item} />)}</div></div></button>)}</div>}
			</div>
		</div>
	)
}

export function TaskDetailPage({ task, onBack }: { task: Task; onBack: () => void }) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'center' }}>
				<WorkspaceHeader eyebrow="Task Detail" title={task.title} description="Standalone task detail page for human review. Task creation and editing remain agent-driven through the API and CLI." />
				<button style={ghostActionButtonStyle} onClick={onBack}>Back to Tasks</button>
			</div>
			<TaskDetail task={task} />
		</div>
	)
}

export function TaskDetail({ task, compact = false }: { task: Task; compact?: boolean }) {
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
					<button style={inlineGhostActionButtonStyle} onClick={() => navigator.clipboard.writeText(task.id)}>Copy ID</button>
				</div>
			</div>
			<div style={{ display: 'grid', gridTemplateColumns: compact ? '1fr 1fr' : '1.2fr 1fr', gap: 16, marginBottom: 16 }}>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Description and metadata</div>
					<div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 12 }}>Task structure, labels, assignee state, and activity should stay explicit near the top of the detail view.</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>{task.labels.map((label) => <LabelChip key={label} label={label} />)}</div>
				</div>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Parent and subtask structure</div>
					<div style={{ display: 'grid', gap: 10 }}>
						<div><div style={metaLabelStyle}>Parent</div><div style={{ fontWeight: 700 }}>{task.parent ?? 'No parent task'}</div></div>
						<div><div style={metaLabelStyle}>Subtasks</div><div style={{ display: 'grid', gap: 6 }}>{task.subtasks?.length ? task.subtasks.map((subtask) => <div key={subtask} style={subtaskRowStyle}>{subtask}</div>) : <div style={{ color: palette.muted }}>No subtasks</div>}</div></div>
					</div>
				</div>
			</div>
			<div style={{ display: 'grid', gridTemplateColumns: compact ? '1fr 1fr' : '1fr 1fr', gap: 16 }}>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Comments</div>
					<div style={{ display: 'grid', gap: 10 }}>{task.comments?.length ? task.comments.map((comment) => <div key={comment.id} style={commentRowStyle}><div style={metaTextStyle}>{comment.updatedAt}</div><div style={{ marginTop: 6, lineHeight: 1.55 }}>{comment.body}</div></div>) : <div style={{ color: palette.muted }}>No comments yet</div>}</div>
				</div>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Recent activity</div>
					<div style={{ display: 'grid', gap: 10 }}>{task.activities?.length ? task.activities.map((activity) => <div key={activity.id} style={activityNoteStyle}><div style={{ fontWeight: 700, marginBottom: 4 }}>{activity.title}</div><div>{activity.detail}</div><div style={{ marginTop: 6, ...metaTextStyle }}>{activity.updatedAt}</div></div>) : <div style={{ color: palette.muted }}>No recent activity</div>}</div>
				</div>
			</div>
		</div>
	)
}

export function PreviewPanel({ preview, tasks, activities, sessions, onClose, onOpenTaskDetail, onDownloadSession }: { preview: PreviewState; tasks: Task[]; activities: ActivityItem[]; sessions: Session[]; onClose: () => void; onOpenTaskDetail: (taskId: string) => void; onDownloadSession: (snapshotId: string) => void }) {
	const content = (() => {
		if (preview?.kind === 'task') {
			const task = tasks.find((item) => item.id === preview.taskId)
			if (!task) return null
			return <><div style={previewHeaderStyle}><div><div style={eyebrowStyle}>Task Preview</div><div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{task.title}</div></div><button style={closeButtonStyle} onClick={onClose}>Close</button></div><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 12 }}><IdPill value={task.id} /><StatusPill status={task.status} /><PriorityPill priority={task.priority} /></div><div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 14 }}>{task.summary}</div><div style={{ display: 'grid', gap: 8, marginBottom: 16 }}><PreviewMeta label="Project" value={task.projectName} /><PreviewMeta label="Assignee" value={task.assignee} /><PreviewMeta label="Updated" value={task.updatedAt} /></div><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 16 }}>{task.labels.map((label) => <LabelChip key={label} label={label} />)}</div><button style={primaryActionButtonStyle} onClick={() => onOpenTaskDetail(task.id)}>Open Full Detail</button></>
		}
		if (preview?.kind === 'activity') {
			const activity = activities.find((item) => item.id === preview.activityId)
			if (!activity) return null
			return <><div style={previewHeaderStyle}><div><div style={eyebrowStyle}>Activity Preview</div><div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{activity.title}</div></div><button style={closeButtonStyle} onClick={onClose}>Close</button></div><div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 16 }}>{activity.detail}</div><div style={{ display: 'grid', gap: 8, marginBottom: 14 }}><PreviewMeta label="Project" value={activity.projectName} /><PreviewMeta label="Task" value={activity.task} /><PreviewMeta label="Updated" value={activity.updatedAt} /></div><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>{activity.labels.map((label) => <LabelChip key={label} label={label} />)}</div></>
		}
		if (preview?.kind === 'session') {
			const session = sessions.find((item) => item.snapshotId === preview.snapshotId)
			if (!session) return null
			return <><div style={previewHeaderStyle}><div><div style={eyebrowStyle}>Session Preview</div><div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{session.title}</div></div><button style={closeButtonStyle} onClick={onClose}>Close</button></div><div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 16 }}>{session.description}</div><div style={{ display: 'grid', gap: 8, marginBottom: 14 }}><PreviewMeta label="Snapshot ID" value={session.snapshotId} mono /><PreviewMeta label="Artifact Name" value={session.artifactName} mono /><PreviewMeta label="Artifact Path" value={session.artifactPath} mono /></div><div style={{ display: 'flex', gap: 8 }}><button style={primaryActionButtonStyle} onClick={() => onDownloadSession(session.snapshotId)}>Download Artifact</button><button style={ghostActionButtonStyle} onClick={() => navigator.clipboard.writeText(session.artifactPath)}>Copy Path</button></div></>
		}
		return null
	})()
	if (!content) return null
	return <div style={previewSurfaceStyle}>{content}</div>
}

export function WorkspaceHeader({ eyebrow, title, description }: { eyebrow: string; title: string; description: string }) {
	return <div style={{ display: 'grid', gap: 6 }}><div style={eyebrowStyle}>{eyebrow}</div><h1 style={{ margin: 0, fontSize: 30, lineHeight: 1.12 }}>{title}</h1><div style={{ color: palette.muted, lineHeight: 1.6, maxWidth: 900 }}>{description}</div></div>
}

function SectionHeading({ title, subtitle }: { title: string; subtitle: string }) {
	return <div style={{ marginBottom: 14 }}><div style={{ fontWeight: 800, fontSize: 18, marginBottom: 4 }}>{title}</div><div style={{ color: palette.muted, lineHeight: 1.55 }}>{subtitle}</div></div>
}

function CompactMetric({ label, value }: { label: string; value: string }) {
	return <div style={{ border: `1px solid ${palette.border}`, borderRadius: 14, background: palette.panel, padding: 14 }}><div style={metaLabelStyle}>{label}</div><div style={{ fontSize: 24, fontWeight: 800, marginTop: 6 }}>{value}</div></div>
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
	return <button onClick={onClick} style={{ borderRadius: 999, padding: '8px 11px', border: `1px solid ${active ? palette.text : palette.border}`, background: active ? palette.text : palette.panel, color: active ? '#fff' : palette.text, fontWeight: 700, cursor: 'pointer' }}>{label}</button>
}

function Pill({ label, color, background }: { label: string; color: string; background: string }) {
	return <span style={{ display: 'inline-flex', alignItems: 'center', padding: '5px 9px', borderRadius: 999, background, color, fontSize: 12, fontWeight: 700 }}>{label}</span>
}

function IdPill({ value }: { value: string }) {
	return <span style={{ display: 'inline-flex', alignItems: 'center', padding: '5px 9px', borderRadius: 999, background: palette.panelAlt, color: palette.text, fontSize: 12, fontWeight: 700, fontFamily: 'ui-monospace, SFMono-Regular, monospace' }}>{value}</span>
}

function PreviewMeta({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
	return <div><div style={metaLabelStyle}>{label}</div><div style={{ fontWeight: 700, fontFamily: mono ? 'ui-monospace, SFMono-Regular, monospace' : undefined, wordBreak: 'break-word' }}>{value}</div></div>
}

function EmptyState({ title, detail }: { title: string; detail: string }) {
	return <div style={{ border: `1px dashed ${palette.borderStrong}`, borderRadius: 16, background: palette.panelAlt, padding: 28, textAlign: 'center' }}><div style={{ fontWeight: 800, fontSize: 18, marginBottom: 6 }}>{title}</div><div style={{ color: palette.muted, lineHeight: 1.6, maxWidth: 600, margin: '0 auto' }}>{detail}</div></div>
}

const previewSurfaceStyle: CSSProperties = { position: 'fixed', top: 24, right: 24, width: 380, maxWidth: 'calc(100vw - 48px)', background: palette.panel, border: `1px solid ${palette.border}`, borderRadius: 18, boxShadow: palette.shadow, padding: 18, zIndex: 30 }
const textInputStyle: CSSProperties = { border: `1px solid ${palette.border}`, borderRadius: 12, padding: '10px 12px', background: palette.panel, color: palette.text, width: '100%', boxSizing: 'border-box' }
