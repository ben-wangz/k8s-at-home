import { ghostActionButtonStyle, primaryActionButtonStyle } from './styles'
import type { ActivityItem, PreviewState, Session, Task } from './types'
import { CopyFeedbackButton, LabelChip, PreviewMeta, PreviewSurface, PriorityPill, StatusPill, closeButtonStyle, eyebrowStyle, previewHeaderStyle } from './view-shared'

export function PreviewPanel({
	preview,
	tasks,
	activities,
	sessions,
	onClose,
	onOpenTaskDetail,
	onDownloadSession,
}: {
	preview: PreviewState
	tasks: Task[]
	activities: ActivityItem[]
	sessions: Session[]
	onClose: () => void
	onOpenTaskDetail: (taskId: string) => void
	onDownloadSession: (snapshotId: string) => void
}) {
	const content = (() => {
		if (preview?.kind === 'task') {
			const task = tasks.find((item) => item.id === preview.taskId)
			if (!task) return null

			return (
				<>
					<div style={previewHeaderStyle}>
						<div>
							<div style={eyebrowStyle}>Task Preview</div>
							<div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{task.title}</div>
						</div>
						<button style={closeButtonStyle} onClick={onClose}>Close</button>
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 12 }}>
						<StatusPill status={task.status} />
						<PriorityPill priority={task.priority} />
					</div>
					<div style={{ color: '#607089', lineHeight: 1.6, marginBottom: 14 }}>{task.summary}</div>
					<div style={{ display: 'grid', gap: 8, marginBottom: 16 }}>
						<PreviewMeta label="Project" value={task.projectName} />
						<PreviewMeta label="Assignee" value={task.assignee} />
						<PreviewMeta label="Updated" value={task.updatedAt} />
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 16 }}>
						{task.labels.map((label) => <LabelChip key={label} label={label} />)}
					</div>
					<button style={primaryActionButtonStyle} onClick={() => onOpenTaskDetail(task.id)}>
						Open Full Detail
					</button>
				</>
			)
		}

		if (preview?.kind === 'activity') {
			const activity = activities.find((item) => item.id === preview.activityId)
			if (!activity) return null

			return (
				<>
					<div style={previewHeaderStyle}>
						<div>
							<div style={eyebrowStyle}>Activity Preview</div>
							<div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{activity.title}</div>
						</div>
						<button style={closeButtonStyle} onClick={onClose}>Close</button>
					</div>
					<div style={{ color: '#607089', lineHeight: 1.6, marginBottom: 16 }}>{activity.detail}</div>
					<div style={{ display: 'grid', gap: 8, marginBottom: 14 }}>
						<PreviewMeta label="Project" value={activity.projectName} />
						<PreviewMeta label="Task" value={activity.task} />
						<PreviewMeta label="Updated" value={activity.updatedAt} />
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
						{activity.labels.map((label) => <LabelChip key={label} label={label} />)}
					</div>
				</>
			)
		}

		if (preview?.kind === 'session') {
			const session = sessions.find((item) => item.snapshotId === preview.snapshotId)
			if (!session) return null

			return (
				<>
					<div style={previewHeaderStyle}>
						<div>
							<div style={eyebrowStyle}>Session Preview</div>
							<div style={{ fontSize: 22, fontWeight: 800, marginTop: 6 }}>{session.title}</div>
						</div>
						<button style={closeButtonStyle} onClick={onClose}>Close</button>
					</div>
					<div style={{ color: '#607089', lineHeight: 1.6, marginBottom: 16 }}>{session.description}</div>
					<div style={{ display: 'grid', gap: 8, marginBottom: 14 }}>
						<PreviewMeta label="Snapshot ID" value={session.snapshotId} mono />
						<PreviewMeta label="Artifact Name" value={session.artifactName} mono />
						<PreviewMeta label="Artifact Path" value={session.artifactPath} mono />
					</div>
					<div style={{ display: 'flex', gap: 8 }}>
						<button style={primaryActionButtonStyle} onClick={() => onDownloadSession(session.snapshotId)}>
							Download Artifact
						</button>
						<CopyFeedbackButton value={session.artifactPath} label="Copy Path" style={ghostActionButtonStyle} />
					</div>
				</>
			)
		}

		return null
	})()

	if (!content) return null
	return <PreviewSurface>{content}</PreviewSurface>
}
