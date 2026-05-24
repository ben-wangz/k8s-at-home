import type { CSSProperties } from 'react'

import { palette } from './ui-config'

export { palette }

export const surfaceStyle: CSSProperties = {
	background: palette.panel,
	border: `1px solid ${palette.border}`,
	borderRadius: 18,
	padding: 18,
	boxShadow: palette.shadow,
}

export const toolbarStyle: CSSProperties = {
	display: 'flex',
	justifyContent: 'space-between',
	gap: 14,
	alignItems: 'center',
	flexWrap: 'wrap',
	marginBottom: 14,
}

export const toolbarTitleStyle: CSSProperties = {
	fontWeight: 800,
	fontSize: 18,
}

export const sidebarInfoCardStyle: CSSProperties = {
	border: '1px solid rgba(255,255,255,0.07)',
	borderRadius: 14,
	padding: 12,
	background: 'rgba(255,255,255,0.03)',
	lineHeight: 1.5,
}

export const sidebarLabelStyle: CSSProperties = {
	fontSize: 11,
	textTransform: 'uppercase',
	letterSpacing: 1,
	color: '#7f90a9',
	marginBottom: 6,
}

export const topbarPrimaryButtonStyle: CSSProperties = {
	border: 'none',
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.accent,
	color: '#061422',
	fontWeight: 800,
	cursor: 'pointer',
}

export const topbarGhostButtonStyle: CSSProperties = {
	border: '1px solid rgba(255,255,255,0.1)',
	borderRadius: 12,
	padding: '10px 14px',
	background: 'rgba(255,255,255,0.04)',
	color: '#d9e5f2',
	fontWeight: 700,
	cursor: 'pointer',
}

export const primaryActionButtonStyle: CSSProperties = {
	border: 'none',
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.accent,
	color: '#08111f',
	fontWeight: 800,
	cursor: 'pointer',
}

export const ghostActionButtonStyle: CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.panel,
	color: palette.text,
	fontWeight: 700,
	cursor: 'pointer',
}

export const inlineActionButtonStyle: CSSProperties = {
	border: 'none',
	borderRadius: 10,
	padding: '8px 10px',
	background: palette.accentSoft,
	color: palette.accentStrong,
	fontWeight: 700,
	cursor: 'pointer',
}

export const inlineGhostActionButtonStyle: CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 10,
	padding: '8px 10px',
	background: palette.panel,
	color: palette.text,
	fontWeight: 700,
	cursor: 'pointer',
}

export const clearActionButtonStyle: CSSProperties = {
	border: 'none',
	background: 'transparent',
	color: palette.accentStrong,
	fontWeight: 700,
	cursor: 'pointer',
	padding: '8px 4px',
}

export const segmentedButtonStyle: CSSProperties = {
	borderRadius: 10,
	padding: '9px 12px',
	border: `1px solid ${palette.border}`,
	fontWeight: 700,
	cursor: 'pointer',
}

export const selectStyle: CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 12,
	padding: '10px 12px',
	background: palette.panel,
	color: palette.text,
	minWidth: 150,
}

export const scopePillStyle: CSSProperties = {
	borderRadius: 999,
	padding: '6px 10px',
	background: palette.panelAlt,
	color: palette.muted,
	fontWeight: 700,
	fontSize: 12,
}

export const tableStyle: CSSProperties = {
	width: '100%',
	borderCollapse: 'collapse',
	minWidth: 920,
}

export const tableHeadStyle: CSSProperties = {
	textAlign: 'left',
	padding: '10px 12px',
	fontSize: 12,
	textTransform: 'uppercase',
	letterSpacing: 0.7,
	color: palette.subtle,
	borderBottom: `1px solid ${palette.border}`,
}

export const tableCellStyle: CSSProperties = {
	padding: '13px 12px',
	borderBottom: `1px solid ${palette.border}`,
	verticalAlign: 'top',
}

export const metaTextStyle: CSSProperties = {
	color: palette.subtle,
	fontSize: 12,
}

export const metaLabelStyle: CSSProperties = {
	color: palette.subtle,
	fontSize: 12,
	textTransform: 'uppercase',
	letterSpacing: 0.6,
	marginBottom: 4,
}

export const eyebrowStyle: CSSProperties = {
	color: palette.accentStrong,
	fontSize: 12,
	textTransform: 'uppercase',
	letterSpacing: 1.2,
	fontWeight: 800,
}

export const ghostFieldStyle: CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 12,
	padding: '10px 14px',
	background: palette.panel,
	color: palette.subtle,
	minWidth: 200,
}

export const listButtonStyle: CSSProperties = {
	textAlign: 'left',
	border: `1px solid ${palette.border}`,
	background: palette.panel,
	borderRadius: 14,
	padding: 14,
	color: palette.text,
	cursor: 'pointer',
}

export const timelineRowStyle: CSSProperties = {
	display: 'grid',
	gridTemplateColumns: '12px minmax(0, 1fr)',
	gap: 10,
	alignItems: 'start',
}

export const timelineButtonStyle: CSSProperties = {
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

export const timelineDotStyle: CSSProperties = {
	width: 10,
	height: 10,
	borderRadius: 999,
	background: palette.accent,
	marginTop: 7,
}

export const registryCardStyle: CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 14,
	padding: 14,
	background: palette.panelAlt,
}

export const boardColumnStyle: CSSProperties = {
	background: palette.panelAlt,
	border: `1px solid ${palette.border}`,
	borderRadius: 16,
	padding: 12,
	minWidth: 280,
	width: 280,
	maxWidth: 280,
	boxSizing: 'border-box',
	alignSelf: 'start',
}

export const boardColumnHeaderStyle: CSSProperties = {
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

export const boardCardStyle: CSSProperties = {
	textAlign: 'left',
	border: `1px solid ${palette.border}`,
	background: palette.panel,
	borderRadius: 14,
	padding: 12,
	color: palette.text,
	cursor: 'pointer',
	width: '100%',
	boxSizing: 'border-box',
	overflow: 'hidden',
}

export const detailBlockStyle: CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 14,
	background: palette.panelAlt,
	padding: 14,
}

export const detailBlockTitleStyle: CSSProperties = {
	fontWeight: 800,
	marginBottom: 10,
}

export const subtaskRowStyle: CSSProperties = {
	borderRadius: 10,
	padding: '10px 12px',
	background: palette.panel,
	border: `1px solid ${palette.border}`,
	fontWeight: 600,
}

export const commentRowStyle: CSSProperties = {
	borderRadius: 12,
	padding: '12px 13px',
	background: palette.panel,
	border: `1px solid ${palette.border}`,
}

export const activityNoteStyle: CSSProperties = {
	borderRadius: 12,
	padding: '12px 13px',
	background: palette.panel,
	border: `1px solid ${palette.border}`,
	color: palette.muted,
	lineHeight: 1.55,
}

export const previewHeaderStyle: CSSProperties = {
	display: 'flex',
	justifyContent: 'space-between',
	gap: 12,
	alignItems: 'flex-start',
	marginBottom: 14,
}

export const closeButtonStyle: CSSProperties = {
	border: `1px solid ${palette.border}`,
	borderRadius: 10,
	padding: '8px 10px',
	background: palette.panel,
	color: palette.text,
	cursor: 'pointer',
	fontWeight: 700,
}
