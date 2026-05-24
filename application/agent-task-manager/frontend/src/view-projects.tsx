import { ghostActionButtonStyle, inlineActionButtonStyle, inlineGhostActionButtonStyle, tableCellStyle, tableHeadStyle, tableStyle, textInputStyle, toolbarStyle, toolbarTitleStyle, surfaceStyle } from './styles'
import type { Project } from './types'
import { EmptyState, WorkspaceHeader, palette } from './view-shared'

export function ProjectsView({ projects, selectedProject, searchValue, onSearchValueChange, onSearch, onSelectProject }: { projects: Project[]; selectedProject: string; searchValue: string; onSearchValueChange: (value: string) => void; onSearch: () => void; onSelectProject: (slug: string) => void }) {
	return (
		<div style={{ display: 'grid', gap: 18 }}>
			<WorkspaceHeader eyebrow="Projects" title="Dense project index with fast scope switching" description="Projects use a table-first pattern instead of oversized card walls. Selecting a project should move directly into task work." />
			<div style={surfaceStyle}>
				<div style={toolbarStyle}><div style={toolbarTitleStyle}>Project Registry</div><div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}><input value={searchValue} onChange={(event) => onSearchValueChange(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') onSearch() }} placeholder="Search project name" style={textInputStyle} /><button style={ghostActionButtonStyle} onClick={onSearch}>GO</button></div></div>
				<div style={{ overflowX: 'auto' }}>
					{projects.length === 0 && <EmptyState title="No projects match this search" detail="Refine the project name query or clear it to see all projects sorted by project name." />}
					{projects.length > 0 && <table style={tableStyle}><thead><tr>{['Project', 'Slug', 'Open', 'In Progress', 'In Review', 'Done', 'Cancelled', 'Updated', 'Actions'].map((header) => <th key={header} style={tableHeadStyle}>{header}</th>)}</tr></thead><tbody>{projects.map((project) => { const active = project.slug === selectedProject; return <tr key={project.id} style={{ background: active ? palette.accentSoft : 'transparent' }}><td style={tableCellStyle}><div style={{ fontWeight: 700 }}>{project.name}</div><div style={{ color: palette.muted, fontSize: 13, marginTop: 4 }}>{project.summary}</div></td><td style={{ ...tableCellStyle, fontFamily: 'ui-monospace, SFMono-Regular, monospace' }}>{project.slug}</td><td style={tableCellStyle}>{project.open}</td><td style={tableCellStyle}>{project.inProgress}</td><td style={tableCellStyle}>{project.inReview}</td><td style={tableCellStyle}>{project.done}</td><td style={tableCellStyle}>{project.cancelled}</td><td style={tableCellStyle}>{project.updatedAt}</td><td style={tableCellStyle}><div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}><button style={inlineActionButtonStyle} onClick={() => onSelectProject(project.slug)}>Open Tasks</button><button style={inlineGhostActionButtonStyle} onClick={() => navigator.clipboard.writeText(project.id)}>Copy ID</button></div></td></tr> })}</tbody></table>}
				</div>
			</div>
		</div>
	)
}
