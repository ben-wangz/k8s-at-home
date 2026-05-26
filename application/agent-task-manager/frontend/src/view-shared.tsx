import { useEffect, useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'

import { priorityMeta, statusMeta } from './ui-config'
import {
	closeButtonStyle,
	metaLabelStyle,
	palette,
	previewHeaderStyle,
	registryCardStyle,
	timelineDotStyle,
	timelineRowStyle,
} from './styles'
import type { Priority, TaskStatus } from './types'

export function WorkspaceHeader({ eyebrow, title, description }: { eyebrow: string; title: string; description: string }) {
	return (
		<div style={{ display: 'grid', gap: 6 }}>
			<div style={eyebrowStyle}>{eyebrow}</div>
			<h1 style={{ margin: 0, fontSize: 30, lineHeight: 1.12 }}>{title}</h1>
			<div style={{ color: palette.muted, lineHeight: 1.6, maxWidth: 900 }}>{description}</div>
		</div>
	)
}

export function SectionHeading({ title, subtitle }: { title: string; subtitle: string }) {
	return (
		<div style={{ marginBottom: 14 }}>
			<div style={{ fontWeight: 800, fontSize: 18, marginBottom: 4 }}>{title}</div>
			<div style={{ color: palette.muted, lineHeight: 1.55 }}>{subtitle}</div>
		</div>
	)
}

export function CompactMetric({ label, value }: { label: string; value: string }) {
	return (
		<div style={{ border: `1px solid ${palette.border}`, borderRadius: 14, background: palette.panel, padding: 14 }}>
			<div style={metaLabelStyle}>{label}</div>
			<div style={{ fontSize: 24, fontWeight: 800, marginTop: 6 }}>{value}</div>
		</div>
	)
}

export function StatusPill({ status }: { status: TaskStatus }) {
	const meta = statusMeta[status]
	return <Pill label={meta.label} color={meta.color} background={meta.soft} />
}

export function PriorityPill({ priority }: { priority: Priority }) {
	const meta = priorityMeta[priority]
	return <Pill label={priority} color={meta.color} background={meta.soft} />
}

export function LabelChip({ label }: { label: string }) {
	return <Pill label={label} color={palette.chipText} background={palette.chip} />
}

export function MetaBadge({ label }: { label: string }) {
	return <Pill label={label} color={palette.muted} background={palette.panelAlt} />
}

export function FilterChip({ label }: { label: string }) {
	return <Pill label={label} color={palette.accentStrong} background={palette.accentSoft} />
}

export function ToggleChip({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
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

export function IdPill({ value }: { value: string }) {
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

export function PreviewMeta({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
	return (
		<div>
			<div style={metaLabelStyle}>{label}</div>
			<div
				style={{
					fontWeight: 700,
					fontFamily: mono ? 'ui-monospace, SFMono-Regular, monospace' : undefined,
					wordBreak: 'break-word',
				}}
			>
				{value}
			</div>
		</div>
	)
}

export function EmptyState({ title, detail }: { title: string; detail: string }) {
	return (
		<div style={{ border: `1px dashed ${palette.borderStrong}`, borderRadius: 16, background: palette.panelAlt, padding: 28, textAlign: 'center' }}>
			<div style={{ fontWeight: 800, fontSize: 18, marginBottom: 6 }}>{title}</div>
			<div style={{ color: palette.muted, lineHeight: 1.6, maxWidth: 600, margin: '0 auto' }}>{detail}</div>
		</div>
	)
}

export function CopyFeedbackButton({ value, label, copiedLabel = 'Copied', style }: { value: string; label: string; copiedLabel?: string; style: CSSProperties }) {
	const [copied, setCopied] = useState(false)

	useEffect(() => {
		if (!copied) {
			return
		}

		const timeout = window.setTimeout(() => setCopied(false), 2000)
		return () => window.clearTimeout(timeout)
	}, [copied])

	async function handleCopy() {
		await navigator.clipboard.writeText(value)
		setCopied(true)
	}

	const copiedStyle: CSSProperties = copied
		? { ...style, background: '#dcfce7', border: '1px solid #86efac', color: '#166534' }
		: style

	return (
		<button style={copiedStyle} onClick={() => { void handleCopy() }}>
			{copied ? copiedLabel : label}
		</button>
	)
}

export function PreviewSurface({ children }: { children: ReactNode }) {
	return <div style={previewSurfaceStyle}>{children}</div>
}

function Pill({ label, color, background }: { label: string; color: string; background: string }) {
	return <span style={{ display: 'inline-flex', alignItems: 'center', padding: '5px 9px', borderRadius: 999, background, color, fontSize: 12, fontWeight: 700 }}>{label}</span>
}

export const eyebrowStyle: CSSProperties = {
	color: palette.accentStrong,
	fontSize: 12,
	textTransform: 'uppercase',
	letterSpacing: 1.2,
	fontWeight: 800,
}

export { closeButtonStyle, metaLabelStyle, palette, previewHeaderStyle, registryCardStyle, timelineDotStyle, timelineRowStyle }

const previewSurfaceStyle: CSSProperties = {
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
}
