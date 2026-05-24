import { listButtonStyle, metaTextStyle, palette, surfaceStyle, timelineRowStyle } from './styles'
import type { ActivityItem, Project, Session, Task } from './types'
import { CompactMetric, IdPill, MetaBadge, SectionHeading, StatusPill, WorkspaceHeader, eyebrowStyle, registryCardStyle, timelineDotStyle } from './view-shared'

export function HomeView({ project, tasks, activities, sessions, onOpenTask, loading = false, error = '' }: { project: Project; tasks: Task[]; activities: ActivityItem[]; sessions: Session[]; onOpenTask: (taskId: string) => void; loading?: boolean; error?: string }) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader eyebrow="Home" title="Current work, recent movement, and session checkpoints" description="A focused landing surface for operators. Counts are secondary to active work and recent changes." />
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
							{tasks.slice(0, 4).map((task) => <button key={task.id} onClick={() => onOpenTask(task.id)} style={listButtonStyle}><div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, marginBottom: 6 }}><div style={{ display: 'flex', alignItems: 'center', gap: 8 }}><IdPill value={task.id} /><StatusPill status={task.status} /></div><div style={metaTextStyle}>{task.updatedAt}</div></div><div style={{ fontWeight: 700, marginBottom: 6 }}>{task.title}</div><div style={{ color: palette.muted, lineHeight: 1.5 }}>{task.summary}</div></button>)}
						</div>
					</div>
					<div style={surfaceStyle}>
						<SectionHeading title="Recent activity" subtitle="Audit-oriented updates with clear project and task context." />
						<div style={{ display: 'grid', gap: 12 }}>
							{activities.length === 0 && <div style={{ color: palette.muted }}>No recent activity yet.</div>}
							{activities.slice(0, 4).map((activity) => <div key={activity.id} style={timelineRowStyle}><div style={timelineDotStyle} /><div><div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, marginBottom: 4 }}><div style={{ fontWeight: 700 }}>{activity.title}</div><div style={metaTextStyle}>{activity.updatedAt}</div></div><div style={{ color: palette.muted, lineHeight: 1.5, marginBottom: 8 }}>{activity.detail}</div><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}><MetaBadge label={activity.projectName} /><MetaBadge label={activity.task} /></div></div></div>)}
						</div>
					</div>
				</div>
				<div style={{ display: 'grid', gap: 18 }}>
					<div style={surfaceStyle}>
						<SectionHeading title="Recent sessions" subtitle="Snapshot registry entries with direct artifact awareness." />
						<div style={{ display: 'grid', gap: 10 }}>
							{sessions.length === 0 && <div style={{ color: palette.muted }}>No recent sessions yet.</div>}
							{sessions.map((session) => <div key={session.snapshotId} style={registryCardStyle}><div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, marginBottom: 6 }}><div style={{ fontWeight: 700 }}>{session.title}</div><div style={metaTextStyle}>{session.updatedAt}</div></div><div style={{ color: palette.muted, fontSize: 13, lineHeight: 1.5, marginBottom: 8 }}>{session.description}</div><div style={{ fontFamily: 'ui-monospace, SFMono-Regular, monospace', fontSize: 12, color: palette.text }}>{session.artifactName}</div></div>)}
						</div>
					</div>
					<div style={surfaceStyle}>
						<SectionHeading title="Design intent" subtitle="This shell favors compact work surfaces over display-first dashboard cards." />
						<div style={{ display: 'grid', gap: 8, color: palette.muted, lineHeight: 1.6 }}><div>Tasks, sessions, and activity use one consistent surface model.</div><div>Context flows into the preview panel instead of fragmenting into isolated pages.</div><div>IDs, labels, timestamps, and states stay easy to scan and copy.</div></div>
					</div>
				</div>
			</div>
		</div>
	)
}
