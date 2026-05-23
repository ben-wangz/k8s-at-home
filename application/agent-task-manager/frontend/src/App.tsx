import { useMemo, useState } from 'react'

type TaskStatus = 'backlog' | 'todo' | 'in_progress' | 'in_review'
type View = 'home' | 'projects' | 'board' | 'sessions' | 'activity' | 'task-detail'

type Task = {
  id: string
  title: string
  status: TaskStatus
  priority: 'P0' | 'P1' | 'P2'
  assignee: string
  labels: string[]
  summary: string
}

type Project = {
  id: string
  name: string
  slug: string
  tasks: number
  sessions: number
  updatedAt: string
  summary: string
}

type ActivityItem = {
  title: string
  detail: string
  updatedAt: string
  sortKey: number
  project: string
  task: string
  labels: string[]
}

const palette = {
  bg: '#0b1020',
  bgAlt: '#0e1424',
  panel: '#131a2d',
  panelAlt: '#1a233b',
  border: '#2a3556',
  text: '#eef3ff',
  muted: '#96a5c6',
  accent: '#7cc7ff',
  accentSoft: '#193652',
  success: '#79d29c',
  warning: '#ffcb77',
  danger: '#ff8a80',
}

const tasks: Task[] = [
  {
    id: 'ATM-101',
    title: 'Rebuild task domain in Go',
    status: 'in_progress',
    priority: 'P0',
    assignee: 'builder-agent',
    labels: ['backend', 'domain'],
    summary: 'Replace the old TypeScript task model with Go-first services and persistence boundaries.',
  },
  {
    id: 'ATM-102',
    title: 'Design agent-first CLI contracts',
    status: 'todo',
    priority: 'P0',
    assignee: 'product-agent',
    labels: ['cli', 'agent'],
    summary: 'Define deterministic command outputs, JSON mode, and task/session workflows for automation.',
  },
  {
    id: 'ATM-103',
    title: 'Implement project dashboard copy-ID affordances',
    status: 'in_review',
    priority: 'P1',
    assignee: 'ui-agent',
    labels: ['frontend', 'ux'],
    summary: 'Task cards and detail views should expose fast task ID copy actions for cross-tool references.',
  },
  {
    id: 'ATM-104',
    title: 'Add task reparenting support',
    status: 'backlog',
    priority: 'P1',
    assignee: 'unassigned',
    labels: ['api', 'tasks'],
    summary: 'Fix a known limitation from the previous system by supporting parent changes after creation.',
  },
]

const selectedTask = {
  ...tasks[0],
  parent: 'ATM-099 Build agent-task-manager backend MVP',
  subtasks: ['ATM-121 Define task repositories', 'ATM-122 Add task activity events'],
  acceptance: [
    'Task CRUD endpoints mapped to Go service boundaries.',
    'Repository interfaces do not leak HTTP concerns.',
    'Status transitions create audit log entries.',
  ],
}

const statusColumns: { key: TaskStatus; label: string }[] = [
  { key: 'backlog', label: 'Backlog' },
  { key: 'todo', label: 'Todo' },
  { key: 'in_progress', label: 'In Progress' },
  { key: 'in_review', label: 'In Review' },
]

const sessions = [
  {
    title: 'OpenCode backend domain pass 1',
    snapshotId: 'b4f84a13-7d8e-4fd6-9a13-e9a7f94b3901',
    description: 'Snapshot imported after backend domain refactor checkpoint.',
    artifactName: 'ses_1bc5ca7a9ffeI2yl8CnNXcBxVk.json',
    artifactPath: 's3://agent-task-manager/sessions/opencode/ses_1bc5ca7a9ffeI2yl8CnNXcBxVk.json',
  },
  {
    title: 'OpenCode board UX review snapshot',
    snapshotId: '0b72c993-0ae4-4d96-a21e-7e0ef6265830',
    description: 'Snapshot saved while evaluating board density and task card layout.',
    artifactName: 'ses_ux_board_review.json',
    artifactPath: '/data/snapshots/opencode/ses_ux_board_review.json',
  },
  {
    title: 'OpenCode CLI contract draft snapshot',
    snapshotId: 'd8f720ce-1d1b-4556-b7bb-c2a52c9a65cb',
    description: 'Snapshot captured before revising agent CLI command contracts.',
    artifactName: 'ses_cli_contract_draft.json',
    artifactPath: 's3://agent-task-manager/sessions/opencode/ses_cli_contract_draft.json',
  },
]

const activities: ActivityItem[] = [
  {
    title: 'builder-agent started session backend-domain-pass-1',
    detail: 'Task ATM-101 is actively being restructured around repositories, services, and activity events.',
    updatedAt: '2 min ago',
    sortKey: 6,
    project: 'agent-task-manager',
    task: 'ATM-101',
    labels: ['backend', 'domain'],
  },
  {
    title: 'ui-agent moved ATM-103 to in_review',
    detail: 'Dashboard card behavior is now ready for product review and refinement.',
    updatedAt: '14 min ago',
    sortKey: 5,
    project: 'agent-task-manager',
    task: 'ATM-103',
    labels: ['frontend', 'ux'],
  },
  {
    title: 'product-agent created ATM-104',
    detail: 'Task reparenting is now treated as a first-class requirement for the new system.',
    updatedAt: '27 min ago',
    sortKey: 4,
    project: 'agent-task-manager',
    task: 'ATM-104',
    labels: ['api', 'tasks'],
  },
  {
    title: 'admin commented on ATM-101',
    detail: 'The API should make parent changes explicit and audit-friendly.',
    updatedAt: '43 min ago',
    sortKey: 3,
    project: 'agent-task-manager',
    task: 'ATM-101',
    labels: ['backend', 'tasks'],
  },
  {
    title: 'product-role worker task linked to umbrella initiative',
    detail: 'The product-role OpenCode worker flow was re-created as a proper subtask under the orchestration initiative.',
    updatedAt: '58 min ago',
    sortKey: 2,
    project: 'openclaw-worker-orchestration',
    task: 'OCW-205',
    labels: ['openclaw', 'opencode', 'orchestration'],
  },
  {
    title: 'migration analysis captured pith parent-linking gap',
    detail: 'A task model issue was recorded after discovering parent linkage cannot be added after task creation.',
    updatedAt: '1 hr ago',
    sortKey: 1,
    project: 'pith-migration-research',
    task: 'PMR-031',
    labels: ['pith', 'task-model', 'ux'],
  },
]

const recentTasks = [
  { ...tasks[2], updatedAt: '5 min ago' },
  { ...tasks[0], updatedAt: '12 min ago' },
  { ...tasks[1], updatedAt: '29 min ago' },
]

const projects: Project[] = [
  {
    id: 'PRJ-001',
    name: 'Agent Task Manager',
    slug: 'agent-task-manager',
    tasks: 24,
    sessions: 3,
    updatedAt: '2 min ago',
    summary: 'Go-based rebuild focused on agent-first task orchestration and explicit auditability.',
  },
  {
    id: 'PRJ-002',
    name: 'OpenClaw Worker Orchestration',
    slug: 'openclaw-worker-orchestration',
    tasks: 11,
    sessions: 2,
    updatedAt: '17 min ago',
    summary: 'Explores dynamic OpenCode worker lifecycle, role packaging, and task assignment flows.',
  },
  {
    id: 'PRJ-003',
    name: 'Pith Migration Research',
    slug: 'pith-migration-research',
    tasks: 7,
    sessions: 1,
    updatedAt: '46 min ago',
    summary: 'Captures feature mapping, gaps, and migration constraints from the previous system.',
  },
]

const activityProjects = Array.from(new Set(activities.map((item) => item.project)))
const activityTasks = Array.from(new Set(activities.map((item) => item.task)))
const activityLabels = Array.from(new Set(activities.flatMap((item) => item.labels)))

const navItems: { id: View; label: string }[] = [
  { id: 'home', label: 'Home' },
  { id: 'projects', label: 'Projects' },
  { id: 'board', label: 'Board' },
  { id: 'sessions', label: 'Sessions' },
  { id: 'activity', label: 'Activity' },
]

const statusColor: Record<TaskStatus, string> = {
  backlog: '#6f83ad',
  todo: '#7cc7ff',
  in_progress: '#ffcb77',
  in_review: '#79d29c',
}

const priorityColor = {
  P0: '#ff8a80',
  P1: '#ffcb77',
  P2: '#96a5c6',
}

const primaryButtonStyle: Record<string, string | number> = {
  border: 'none',
  borderRadius: 12,
  padding: '10px 14px',
  background: palette.accent,
  color: '#08111f',
  fontWeight: 700,
  cursor: 'pointer',
}

const ghostButtonStyle: Record<string, string | number> = {
  border: `1px solid ${palette.border}`,
  borderRadius: 12,
  padding: '10px 14px',
  background: 'transparent',
  color: palette.text,
  fontWeight: 700,
  cursor: 'pointer',
}

export default function App() {
  const [activeView, setActiveView] = useState<View>('home')

  return (
    <div
      style={{
        minHeight: '100vh',
        background: `linear-gradient(180deg, ${palette.bg} 0%, ${palette.bgAlt} 100%)`,
        color: palette.text,
        fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
      }}
    >
      <div style={{ maxWidth: 1500, margin: '0 auto', padding: '28px 20px 40px' }}>
        <header style={{ ...panelStyle({ padding: 24, marginBottom: 20, background: 'linear-gradient(135deg, #16213d 0%, #12182b 100%)' }) }}>
          <div style={{ marginBottom: 18 }}>
            <div>
              <div style={{ color: palette.accent, fontSize: 13, textTransform: 'uppercase', letterSpacing: 1.4, marginBottom: 10 }}>
                Agent Task Manager
              </div>
              <h1 style={{ fontSize: 34, lineHeight: 1.1, margin: 0 }}>Top-level navigation for product review and workflow design</h1>
              <p style={{ color: palette.muted, maxWidth: 760, lineHeight: 1.6, margin: '14px 0 0' }}>
                Use the tabs below to inspect each major product surface independently. Task Detail is intentionally excluded from the top banner and is reached from task links.
              </p>
            </div>
          </div>
          <nav style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
            {navItems.map((item) => {
              const active = activeView === item.id
              return (
                <button
                  key={item.id}
                  onClick={() => setActiveView(item.id)}
                  style={{
                    borderRadius: 999,
                    padding: '10px 16px',
                    border: `1px solid ${active ? palette.accent : palette.border}`,
                    background: active ? palette.accentSoft : 'rgba(255,255,255,0.02)',
                    color: active ? palette.accent : palette.text,
                    cursor: 'pointer',
                    fontWeight: 700,
                  }}
                >
                  {item.label}
                </button>
              )
            })}
          </nav>
        </header>

        {activeView === 'home' && <HomeView onOpenTask={() => setActiveView('task-detail')} />}
        {activeView === 'projects' && <ProjectsView />}
        {activeView === 'board' && <BoardView onOpenTask={() => setActiveView('task-detail')} />}
        {activeView === 'sessions' && <SessionsView />}
        {activeView === 'activity' && <ActivityView />}
        {activeView === 'task-detail' && <TaskDetailView onBack={() => setActiveView('board')} />}
      </div>
    </div>
  )
}

function HomeView({ onOpenTask }: { onOpenTask: () => void }) {
  return (
    <section style={{ display: 'grid', gap: 20 }}>
      <section style={{ ...panelStyle({ padding: 20 }) }}>
        <SectionTitle eyebrow="Home" title="Current Agent Task Manager module overview" />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))', gap: 14 }}>
          <MetricCard label="Projects" value="1" hint="agent-task-manager" />
          <MetricCard label="Tasks" value="24" hint="backlog + active" />
          <MetricCard label="Sessions" value="3" hint="agent-first workflows" />
          <MetricCard label="Activities" value="3" hint="latest updates" />
        </div>
      </section>

      <section style={{ ...panelStyle({ padding: 20 }) }}>
        <SectionTitle eyebrow="Quick Entry" title="Latest updates across tasks, sessions, and activities" />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))', gap: 14 }}>
          <div style={{ display: 'grid', gap: 10 }}>
            <SubsectionTitle title="Tasks" />
            {recentTasks.map((task) => (
              <button
                key={task.id}
                onClick={onOpenTask}
                style={{
                  textAlign: 'left',
                  border: `1px solid ${palette.border}`,
                  background: palette.panelAlt,
                  borderRadius: 14,
                  padding: 14,
                  color: palette.text,
                  cursor: 'pointer',
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8, marginBottom: 6 }}>
                  <div style={{ color: palette.accent, fontSize: 13 }}>{task.id}</div>
                  <div style={{ color: palette.muted, fontSize: 12 }}>{task.updatedAt}</div>
                </div>
                <div style={{ fontWeight: 700, marginBottom: 6 }}>{task.title}</div>
                <div style={{ color: palette.muted }}>{task.summary}</div>
              </button>
            ))}
          </div>

          <div style={{ display: 'grid', gap: 10 }}>
            <SubsectionTitle title="Sessions" />
            {sessions.map((session) => (
              <div
                key={session.snapshotId}
                style={{
                  border: `1px solid ${palette.border}`,
                  background: palette.panelAlt,
                  borderRadius: 14,
                  padding: 14,
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8, marginBottom: 6 }}>
                  <div style={{ fontWeight: 700 }}>{session.title}</div>
                  <div style={{ color: palette.muted, fontSize: 12 }}>{session.snapshotId.slice(0, 8)}</div>
                </div>
                <div style={{ color: palette.muted, fontSize: 13, marginBottom: 6 }}>{session.description}</div>
                <div style={{ color: '#dce6ff', fontSize: 13 }}>{session.artifactName}</div>
              </div>
            ))}
          </div>

          <div style={{ display: 'grid', gap: 10 }}>
            <SubsectionTitle title="Activities" />
            {activities.slice(0, 3).map((activity) => (
              <div
                key={activity.title}
                style={{
                  border: `1px solid ${palette.border}`,
                  background: palette.panelAlt,
                  borderRadius: 14,
                  padding: 14,
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8, marginBottom: 6 }}>
                  <div style={{ fontWeight: 700 }}>{activity.title}</div>
                  <div style={{ color: palette.muted, fontSize: 12 }}>{activity.updatedAt}</div>
                </div>
                <div style={{ color: palette.muted, lineHeight: 1.6 }}>{activity.detail}</div>
              </div>
            ))}
          </div>
        </div>
      </section>
    </section>
  )
}

function BoardView({ onOpenTask }: { onOpenTask: () => void }) {
  return (
    <section style={{ display: 'grid', gap: 16 }}>
      <div style={{ ...panelStyle({ padding: 20 }) }}>
        <SectionTitle eyebrow="Board" title="Independent board page" />
        <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginTop: 8 }}>
          <button style={boardToolbarTabStyle(true)}>Board</button>
          <button style={boardToolbarTabStyle(false)}>List</button>
          <div style={{ width: 1, background: palette.border, margin: '0 6px' }} />
          <button style={boardFilterStyle}>All statuses</button>
          <button style={boardFilterStyle}>All priorities</button>
        </div>
      </div>

      <div style={{ display: 'flex', gap: 12, overflowX: 'auto', paddingBottom: 6 }}>
        {statusColumns.map((column) => {
          const columnTasks = tasks.filter((task) => task.status === column.key)
          return (
            <div
              key={column.key}
              style={{
                minWidth: 295,
                maxWidth: 295,
                background: '#151d32',
                border: `1px solid ${palette.border}`,
                borderRadius: 16,
                padding: 0,
              }}
            >
              <div
                style={{
                  padding: '12px 14px',
                  fontSize: 11,
                  fontWeight: 700,
                  textTransform: 'uppercase',
                  letterSpacing: 0.8,
                  color: palette.muted,
                  display: 'flex',
                  justifyContent: 'space-between',
                  alignItems: 'center',
                  borderBottom: `1px solid ${palette.border}`,
                }}
              >
                <div>{column.label}</div>
                <div
                  style={{
                    background: '#202a44',
                    color: palette.muted,
                    borderRadius: 999,
                    padding: '2px 9px',
                    fontSize: 11,
                    fontWeight: 700,
                  }}
                >
                  {columnTasks.length}
                </div>
              </div>
              <div style={{ padding: 6, display: 'grid', gap: 6 }}>
                {columnTasks.map((task) => (
                  <article
                    key={task.id}
                    style={{
                      background: '#0f1628',
                      border: '1px solid rgba(255,255,255,0.05)',
                      borderRadius: 12,
                      padding: '12px 13px',
                      boxShadow: '0 6px 18px rgba(0, 0, 0, 0.14)',
                    }}
                  >
                    <div style={{ marginBottom: 8 }}>
                      <button onClick={onOpenTask} style={{ ...taskLinkStyle, fontSize: 15, lineHeight: 1.45, color: palette.text, fontWeight: 600 }}>
                        {task.title}
                      </button>
                    </div>
                    <div style={{ color: palette.muted, fontSize: 13, lineHeight: 1.5, marginBottom: 10 }}>{task.summary}</div>
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 10 }}>
                      <span
                        style={{
                          display: 'inline-flex',
                          alignItems: 'center',
                          padding: '2px 8px',
                          borderRadius: 999,
                          fontSize: 11,
                          fontWeight: 700,
                          background: `${priorityColor[task.priority]}22`,
                          color: priorityColor[task.priority],
                        }}
                      >
                        {task.priority}
                      </span>
                      {task.labels.map((label) => (
                        <span
                          key={label}
                          style={{
                            fontSize: 11,
                            color: palette.muted,
                            border: `1px solid rgba(255,255,255,0.06)`,
                            borderRadius: 999,
                            padding: '3px 8px',
                          }}
                        >
                          {label}
                        </span>
                      ))}
                    </div>
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8, marginTop: 4 }}>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                        <span style={{ color: palette.accent, fontSize: 12 }}>{task.id}</span>
                        <span style={{ color: palette.muted, fontSize: 12 }}>{task.assignee}</span>
                      </div>
                      <button style={compactGhostButtonStyle}>Copy ID</button>
                    </div>
                  </article>
                ))}
              </div>
            </div>
          )
        })}
      </div>
    </section>
  )
}

function ProjectsView() {
  return (
    <section style={{ display: 'grid', gap: 20 }}>
      <section style={{ ...panelStyle({ padding: 20 }) }}>
        <SectionTitle eyebrow="Projects" title="Independent project index page" />
        <div style={{ color: palette.muted, lineHeight: 1.7, maxWidth: 900 }}>
          This view lists all projects in the system. It is the place to scan portfolio-wide work, compare load across projects, and choose which project to enter next.
        </div>
      </section>

      <section style={{ ...panelStyle({ padding: 20 }) }}>
        <div style={{ display: 'grid', gap: 12 }}>
          {projects.map((project) => (
            <article
              key={project.id}
              style={{
                display: 'grid',
                gridTemplateColumns: '180px 1.8fr 100px 100px 120px',
                gap: 14,
                alignItems: 'center',
                border: `1px solid ${palette.border}`,
                background: palette.panelAlt,
                borderRadius: 14,
                padding: 16,
              }}
            >
              <div>
                <div style={{ color: palette.accent, fontSize: 13, marginBottom: 6 }}>{project.id}</div>
                <div style={{ color: palette.muted, fontSize: 13 }}>{project.slug}</div>
              </div>
              <div>
                <div style={{ fontWeight: 700, marginBottom: 6 }}>{project.name}</div>
                <div style={{ color: palette.muted, lineHeight: 1.6, fontSize: 14 }}>{project.summary}</div>
              </div>
              <div>
                <div style={{ fontSize: 12, color: palette.muted, marginBottom: 4 }}>Tasks</div>
                <div style={{ fontWeight: 800 }}>{project.tasks}</div>
              </div>
              <div>
                <div style={{ fontSize: 12, color: palette.muted, marginBottom: 4 }}>Sessions</div>
                <div style={{ fontWeight: 800 }}>{project.sessions}</div>
              </div>
              <div>
                <div style={{ fontSize: 12, color: palette.muted, marginBottom: 4 }}>Updated</div>
                <div style={{ fontWeight: 700 }}>{project.updatedAt}</div>
              </div>
            </article>
          ))}
        </div>
      </section>
    </section>
  )
}

function SessionsView() {
  return (
    <section style={{ display: 'grid', gap: 20 }}>
      <section style={{ ...panelStyle({ padding: 20 }) }}>
        <SectionTitle eyebrow="Sessions" title="OpenCode session snapshot registry" />
        <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', marginBottom: 12 }}>
          <button style={primaryButtonStyle}>Upload Session Export</button>
          <button style={ghostButtonStyle}>Download Selected Export</button>
        </div>
        <div style={{ color: palette.muted, lineHeight: 1.7 }}>
          This page manages exported OpenCode session snapshot artifacts. The UI focuses on artifact upload/download and minimal identifying metadata for each snapshot.
        </div>
      </section>

      <section style={{ ...panelStyle({ padding: 20 }) }}>
        <div style={{ display: 'grid', gap: 12 }}>
          {sessions.map((session) => (
            <div key={session.snapshotId} style={{ background: palette.panelAlt, borderRadius: 14, border: `1px solid ${palette.border}`, padding: 16 }}>
              <div style={{ display: 'grid', gridTemplateColumns: '1.4fr 1fr 1.1fr', gap: 16, alignItems: 'start' }}>
                <div>
                  <div style={{ fontWeight: 700, marginBottom: 8 }}>{session.title}</div>
                  <div style={{ color: palette.muted, lineHeight: 1.6 }}>{session.description}</div>
                </div>
                <div>
                  <div style={{ fontSize: 12, color: palette.muted, marginBottom: 6 }}>Snapshot UUID</div>
                  <div style={{ fontWeight: 700, lineHeight: 1.5 }}>{session.snapshotId}</div>
                </div>
                <div>
                  <div style={{ fontSize: 12, color: palette.muted, marginBottom: 6 }}>Export Artifact</div>
                  <div style={{ fontWeight: 700, marginBottom: 6 }}>{session.artifactName}</div>
                  <div style={{ color: palette.muted, lineHeight: 1.6, fontSize: 13 }}>{session.artifactPath}</div>
                </div>
              </div>
              <div style={{ display: 'flex', gap: 10, marginTop: 14 }}>
                <button style={ghostButtonStyle}>Download</button>
                <button style={ghostButtonStyle}>Copy Path</button>
              </div>
            </div>
          ))}
        </div>
      </section>
    </section>
  )
}

function ActivityView() {
  const [projectFilter, setProjectFilter] = useState('')
  const [projectNot, setProjectNot] = useState(false)
  const [taskFilter, setTaskFilter] = useState('')
  const [taskNot, setTaskNot] = useState(false)
  const [labelFilter, setLabelFilter] = useState('')
  const [labelNot, setLabelNot] = useState(false)
  const [sortOrder, setSortOrder] = useState<'desc' | 'asc'>('desc')

  const filteredActivities = useMemo(() => {
    const filtered = activities.filter((item) => {
      const projectMatch = projectFilter ? item.project === projectFilter : true
      const taskMatch = taskFilter ? item.task === taskFilter : true
      const labelMatch = labelFilter ? item.labels.includes(labelFilter) : true

      const finalProjectMatch = projectFilter ? (projectNot ? !projectMatch : projectMatch) : true
      const finalTaskMatch = taskFilter ? (taskNot ? !taskMatch : taskMatch) : true
      const finalLabelMatch = labelFilter ? (labelNot ? !labelMatch : labelMatch) : true

      return finalProjectMatch && finalTaskMatch && finalLabelMatch
    })

    return filtered.sort((left, right) => {
      if (sortOrder === 'asc') {
        return left.sortKey - right.sortKey
      }
      return right.sortKey - left.sortKey
    })
  }, [projectFilter, projectNot, taskFilter, taskNot, labelFilter, labelNot, sortOrder])

  return (
    <section style={{ ...panelStyle({ padding: 20 }) }}>
      <SectionTitle eyebrow="Activity" title="Independent activity page for later deep design" />
      <div style={{ display: 'grid', gap: 12, marginBottom: 18 }}>
        <div style={{ color: palette.muted, lineHeight: 1.7 }}>
          Filter this timeline by project, task, and label. Filters can be combined, and each filter supports a `NOT` mode for exclusion.
        </div>
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(4, minmax(0, 1fr))',
            gap: 12,
            background: palette.panelAlt,
            border: `1px solid ${palette.border}`,
            borderRadius: 14,
            padding: 14,
          }}
        >
          <FilterBlock
            title="Project"
            value={projectFilter}
            onChange={setProjectFilter}
            notValue={projectNot}
            onToggleNot={() => setProjectNot((value) => !value)}
            options={activityProjects}
          />
          <FilterBlock
            title="Task"
            value={taskFilter}
            onChange={setTaskFilter}
            notValue={taskNot}
            onToggleNot={() => setTaskNot((value) => !value)}
            options={activityTasks}
          />
          <FilterBlock
            title="Label"
            value={labelFilter}
            onChange={setLabelFilter}
            notValue={labelNot}
            onToggleNot={() => setLabelNot((value) => !value)}
            options={activityLabels}
          />
          <div style={{ display: 'grid', gap: 8 }}>
            <div style={{ fontSize: 13, fontWeight: 700 }}>Sort</div>
            <div style={{ display: 'flex', gap: 8 }}>
              <button onClick={() => setSortOrder('desc')} style={sortOrder === 'desc' ? activeSortButtonStyle : inactiveSortButtonStyle}>
                Descending
              </button>
              <button onClick={() => setSortOrder('asc')} style={sortOrder === 'asc' ? activeSortButtonStyle : inactiveSortButtonStyle}>
                Ascending
              </button>
            </div>
            <div style={{ color: palette.muted, fontSize: 12 }}>Sort by activity time</div>
          </div>
        </div>
        <div style={{ color: palette.muted, fontSize: 13 }}>Showing {filteredActivities.length} matching activities</div>
      </div>
      <div style={{ display: 'grid', gap: 12 }}>
        {filteredActivities.map((item) => (
          <div key={item.title} style={{ display: 'flex', gap: 12, background: palette.panelAlt, borderRadius: 14, border: `1px solid ${palette.border}`, padding: 14 }}>
            <div style={{ width: 10, height: 10, borderRadius: 999, background: palette.accent, marginTop: 8, flex: '0 0 auto' }} />
            <div>
              <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, marginBottom: 6 }}>
                <div style={{ fontWeight: 700 }}>{item.title}</div>
                <div style={{ color: palette.muted, fontSize: 12 }}>{item.updatedAt}</div>
              </div>
              <div style={{ color: palette.muted, lineHeight: 1.6 }}>{item.detail}</div>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 10 }}>
                <span style={metaPillStyle}>{item.project}</span>
                <span style={metaPillStyle}>{item.task}</span>
                {item.labels.map((label) => (
                  <span key={label} style={metaPillStyle}>
                    {label}
                  </span>
                ))}
              </div>
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}

function FilterBlock({
  title,
  value,
  onChange,
  notValue,
  onToggleNot,
  options,
}: {
  title: string
  value: string
  onChange: (value: string) => void
  notValue: boolean
  onToggleNot: () => void
  options: string[]
}) {
  return (
    <div style={{ display: 'grid', gap: 8 }}>
      <div style={{ fontSize: 13, fontWeight: 700 }}>{title}</div>
      <select value={value} onChange={(event) => onChange(event.target.value)} style={filterSelectStyle}>
        <option value="">All</option>
        {options.map((option) => (
          <option key={option} value={option}>
            {option}
          </option>
        ))}
      </select>
      <button onClick={onToggleNot} style={notValue ? activeNotButtonStyle : inactiveNotButtonStyle}>
        NOT
      </button>
    </div>
  )
}

function TaskDetailView({ onBack }: { onBack: () => void }) {
  return (
    <section style={{ display: 'grid', gap: 20 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12 }}>
        <button onClick={onBack} style={ghostButtonStyle}>
          Back to Board
        </button>
        <div style={{ color: palette.muted }}>Task Detail is intentionally reached from task links, not the top banner.</div>
      </div>

      <section style={{ ...panelStyle({ padding: 20 }) }}>
        <SectionTitle eyebrow="Task Detail" title={selectedTask.title} />
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 12 }}>
          <span style={pillStyle(palette.accent, palette.accentSoft)}>{selectedTask.id}</span>
          <span style={pillStyle(statusColor[selectedTask.status], '#1d253d')}>{selectedTask.status}</span>
          <span style={pillStyle(priorityColor[selectedTask.priority], '#2a1e1d')}>{selectedTask.priority}</span>
        </div>
        <DetailRow label="Parent" value={selectedTask.parent} />
        <DetailRow label="Assignee" value={selectedTask.assignee} />
        <DetailRow label="Labels" value={selectedTask.labels.join(', ')} />
        <div style={{ marginTop: 16 }}>
          <div style={{ fontSize: 12, textTransform: 'uppercase', letterSpacing: 1.2, color: palette.muted, marginBottom: 8 }}>Summary</div>
          <div style={{ lineHeight: 1.6, color: '#dce6ff' }}>{selectedTask.summary}</div>
        </div>
        <div style={{ marginTop: 16 }}>
          <div style={{ fontSize: 12, textTransform: 'uppercase', letterSpacing: 1.2, color: palette.muted, marginBottom: 8 }}>Acceptance</div>
          <ul style={{ margin: 0, paddingLeft: 18, color: '#dce6ff', lineHeight: 1.8 }}>
            {selectedTask.acceptance.map((item) => (
              <li key={item}>{item}</li>
            ))}
          </ul>
        </div>
        <div style={{ marginTop: 16 }}>
          <div style={{ fontSize: 12, textTransform: 'uppercase', letterSpacing: 1.2, color: palette.muted, marginBottom: 8 }}>Subtasks</div>
          <div style={{ display: 'grid', gap: 8 }}>
            {selectedTask.subtasks.map((subtask) => (
              <div key={subtask} style={{ background: palette.panelAlt, borderRadius: 12, border: `1px solid ${palette.border}`, padding: 10 }}>
                {subtask}
              </div>
            ))}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 10, marginTop: 18, flexWrap: 'wrap' }}>
          <button style={primaryButtonStyle}>Copy ID</button>
        </div>
      </section>
    </section>
  )
}

function SectionTitle({ eyebrow, title }: { eyebrow: string; title: string }) {
  return (
    <div style={{ marginBottom: 16 }}>
      <div style={{ color: palette.muted, fontSize: 12, textTransform: 'uppercase', letterSpacing: 1.2, marginBottom: 6 }}>{eyebrow}</div>
      <h2 style={{ fontSize: 22, margin: 0 }}>{title}</h2>
    </div>
  )
}

function MetricCard({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div style={{ background: 'rgba(255,255,255,0.03)', border: `1px solid ${palette.border}`, borderRadius: 16, padding: 16 }}>
      <div style={{ color: palette.muted, fontSize: 12, textTransform: 'uppercase', letterSpacing: 1.1 }}>{label}</div>
      <div style={{ fontSize: 30, fontWeight: 800, margin: '8px 0 4px' }}>{value}</div>
      <div style={{ color: palette.muted, fontSize: 13 }}>{hint}</div>
    </div>
  )
}

function FeatureCard({ title, text }: { title: string; text: string }) {
  return (
    <div style={{ background: palette.panelAlt, border: `1px solid ${palette.border}`, borderRadius: 16, padding: 16 }}>
      <div style={{ fontWeight: 700, marginBottom: 8 }}>{title}</div>
      <div style={{ color: palette.muted, lineHeight: 1.6, fontSize: 14 }}>{text}</div>
    </div>
  )
}

function DetailRow({ label, value }: { label: string; value: string }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, padding: '8px 0', borderBottom: `1px solid ${palette.border}` }}>
      <span style={{ color: palette.muted }}>{label}</span>
      <span style={{ textAlign: 'right' }}>{value}</span>
    </div>
  )
}

function SubsectionTitle({ title }: { title: string }) {
  return <div style={{ fontSize: 14, fontWeight: 800, letterSpacing: 0.3 }}>{title}</div>
}

function panelStyle(extra?: Record<string, string | number>) {
  return {
    background: palette.panel,
    border: `1px solid ${palette.border}`,
    borderRadius: 18,
    boxShadow: '0 20px 50px rgba(0, 0, 0, 0.22)',
    ...extra,
  }
}

function pillStyle(color: string, background: string) {
  return {
    display: 'inline-flex',
    alignItems: 'center',
    padding: '4px 9px',
    borderRadius: 999,
    fontSize: 12,
    color,
    background,
    border: `1px solid ${palette.border}`,
  }
}

const taskLinkStyle: Record<string, string | number> = {
  border: 'none',
  background: 'transparent',
  padding: 0,
  margin: 0,
  color: palette.accent,
  cursor: 'pointer',
  fontWeight: 700,
  textAlign: 'left',
}

const compactGhostButtonStyle: Record<string, string | number> = {
  border: `1px solid ${palette.border}`,
  borderRadius: 10,
  padding: '6px 10px',
  background: 'transparent',
  color: palette.text,
  fontWeight: 700,
  fontSize: 12,
  cursor: 'pointer',
}

function boardToolbarTabStyle(active: boolean) {
  return {
    borderRadius: 999,
    padding: '8px 14px',
    border: `1px solid ${active ? palette.accent : palette.border}`,
    background: active ? palette.accentSoft : 'transparent',
    color: active ? palette.accent : palette.text,
    cursor: 'pointer',
    fontWeight: 700,
    fontSize: 13,
  }
}

const boardFilterStyle: Record<string, string | number> = {
  borderRadius: 10,
  padding: '8px 12px',
  border: `1px solid ${palette.border}`,
  background: 'transparent',
  color: palette.muted,
  cursor: 'pointer',
  fontWeight: 600,
  fontSize: 13,
}

const filterSelectStyle: Record<string, string | number> = {
  borderRadius: 10,
  padding: '10px 12px',
  border: `1px solid ${palette.border}`,
  background: '#0f1628',
  color: palette.text,
}

const inactiveNotButtonStyle: Record<string, string | number> = {
  borderRadius: 10,
  padding: '8px 12px',
  border: `1px solid ${palette.border}`,
  background: 'transparent',
  color: palette.muted,
  fontWeight: 700,
  cursor: 'pointer',
}

const activeNotButtonStyle: Record<string, string | number> = {
  ...inactiveNotButtonStyle,
  background: '#2d1b1b',
  color: palette.danger,
  border: `1px solid ${palette.danger}`,
}

const metaPillStyle: Record<string, string | number> = {
  display: 'inline-flex',
  alignItems: 'center',
  padding: '4px 8px',
  borderRadius: 999,
  fontSize: 12,
  color: palette.muted,
  border: `1px solid ${palette.border}`,
}

const inactiveSortButtonStyle: Record<string, string | number> = {
  borderRadius: 10,
  padding: '8px 12px',
  border: `1px solid ${palette.border}`,
  background: 'transparent',
  color: palette.muted,
  fontWeight: 700,
  cursor: 'pointer',
}

const activeSortButtonStyle: Record<string, string | number> = {
  ...inactiveSortButtonStyle,
  background: palette.accentSoft,
  color: palette.accent,
  border: `1px solid ${palette.accent}`,
}
