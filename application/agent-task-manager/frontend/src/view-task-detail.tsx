import {
	activityNoteStyle,
	commentRowStyle,
	detailBlockStyle,
	detailBlockTitleStyle,
	ghostActionButtonStyle,
	inlineGhostActionButtonStyle,
	metaTextStyle,
	palette,
	subtaskRowStyle,
	surfaceStyle,
} from './styles'
import type { Task } from './types'
import { CopyFeedbackButton, IdPill, LabelChip, MetaBadge, PriorityPill, StatusPill, WorkspaceHeader, eyebrowStyle, metaLabelStyle } from './view-shared'

export function TaskDetailPage({ task, onBack }: { task: Task; onBack: () => void }) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'center' }}>
				<WorkspaceHeader
					eyebrow="Task Detail"
					title={task.title}
					description="Standalone task detail page for human review. Task creation and editing remain agent-driven through the API and CLI."
				/>
				<button style={ghostActionButtonStyle} onClick={onBack}>
					Back to Tasks
				</button>
			</div>
			<TaskDetail task={task} />
		</div>
	)
}

export function TaskDetail({ task, compact = false }: { task: Task; compact?: boolean }) {
	return (
		<div style={{ ...surfaceStyle, padding: compact ? 18 : 22, gap: 0 }}>
			<div
				style={{
					display: 'flex',
					justifyContent: 'space-between',
					gap: 16,
					alignItems: 'flex-start',
					flexWrap: 'wrap',
					marginBottom: 16,
				}}
			>
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
					<CopyFeedbackButton value={task.id} label="Copy ID" style={inlineGhostActionButtonStyle} />
				</div>
			</div>

			<div style={{ display: 'grid', gridTemplateColumns: compact ? '1fr 1fr' : '1.2fr 1fr', gap: 16, marginBottom: 16 }}>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Description and metadata</div>
					<div style={{ color: palette.muted, lineHeight: 1.6, marginBottom: 12 }}>
						Task structure, labels, assignee state, and activity should stay explicit near the top of the detail view.
					</div>
					<div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
						{task.labels.map((label) => <LabelChip key={label} label={label} />)}
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
								{task.subtasks?.length
									? task.subtasks.map((subtask) => <div key={subtask} style={subtaskRowStyle}>{subtask}</div>)
									: <div style={{ color: palette.muted }}>No subtasks</div>}
							</div>
						</div>
					</div>
				</div>
			</div>

			<div style={{ display: 'grid', gridTemplateColumns: compact ? '1fr' : '1fr 1fr', gap: 16 }}>
				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Comments</div>
					<div style={{ display: 'grid', gap: 10 }}>
						{task.comments?.length
							? task.comments.map((comment) => (
								<div key={comment.id} style={commentRowStyle}>
									<div style={metaTextStyle}>{comment.updatedAt}</div>
									<div style={{ marginTop: 6, lineHeight: 1.55 }}>{comment.body}</div>
								</div>
							))
							: <div style={{ color: palette.muted }}>No comments yet</div>}
					</div>
				</div>

				<div style={detailBlockStyle}>
					<div style={detailBlockTitleStyle}>Recent activity</div>
					<div style={{ display: 'grid', gap: 10 }}>
						{task.activities?.length
							? task.activities.map((activity) => (
								<div key={activity.id} style={activityNoteStyle}>
									<div style={{ fontWeight: 700, marginBottom: 4 }}>{activity.title}</div>
									<div>{activity.detail}</div>
									<div style={{ marginTop: 6, ...metaTextStyle }}>{activity.updatedAt}</div>
								</div>
							))
							: <div style={{ color: palette.muted }}>No recent activity</div>}
					</div>
				</div>
			</div>
		</div>
	)
}
