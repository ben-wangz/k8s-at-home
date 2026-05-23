# Agent Task Manager UI Redesign

## Goal

This document replaces the current frontend mock direction.

The earlier UI was useful only for confirming feature scope. It is not a good foundation for implementation. The product now needs a clearer, more focused application UI that supports daily task execution, audit review, and agent-assisted workflows without feeling like a demo screen.

This redesign is informed by the `opencode/skills/ui-design-skill/` application UI guidance and adapts it to the actual product constraints of `agent-task-manager`.

## Product Context

### Page type

- Application UI / dashboard

### Main goal

- Help users view information and complete task-management workflows efficiently

### Target audience

- Internal teams
- Developers and operators
- Agent-heavy technical users

## Selected UI Direction

### Product posture

- Efficiency-first work application, not a marketing-style dashboard
- Serious operational tone, but not visually dead or generic
- Strong desktop experience with solid mobile fallback, not mobile-first

### Layout direction

- Left navigation plus main content
- Main content can open contextual right-side detail panels where helpful
- Avoid top-banner tab navigation as the primary structure

### Workspace direction

- Mixed workspace
- Board, list, detail, timeline, and snapshot registry each need different working surfaces
- Workflow matters more than ornamental metrics

### Density direction

- Compact but clean
- Higher information density than the current mock
- Tighter spacing, stronger alignment, clearer hierarchy

### Visual system direction

- Deep gray and cool white base, not theatrical dark gradients
- One restrained cool accent color for interaction and state emphasis
- Sans-serif typography with a technical product feel
- Minimal decorative imagery; use layout, typography, and data surfaces as the visual identity

### Motion direction

- Very subtle motion only
- Fast hover, focus, panel, and sorting feedback
- No decorative animation or splashy transitions

## Core Design Problems In The Current Mock

### 1. Navigation is presentation-first, not workflow-first

The top-banner tab layout feels like a concept deck rather than an application shell.

### 2. Information hierarchy is weak

Everything is presented as similarly weighted cards and panels, so users do not immediately understand where to start, what is active, and what is secondary.

### 3. Board, task detail, sessions, and activity do not feel like parts of one coherent system

They look like separate demo pages instead of one product with consistent interaction patterns.

### 4. The UI uses too much decorative visual treatment for a work tool

Large gradients, oversized hero framing, and heavy panel treatment reduce clarity and waste space.

### 5. The current design is not agent-task-manager specific

It does not yet express the product's core identity: agent-friendly task execution, strong auditability, explicit task structure, and session artifact handling.

## Design Principles

### 1. Work surface first

Every screen should prioritize actual work: filtering, scanning, editing, linking, reviewing, and exporting.

### 2. Stable shell, variable workspace

The application frame should stay stable while the main workspace changes by view.

### 3. Detail should be close to context

Where possible, use inline expansion, side panels, or drawers instead of forcing full-page navigation for every detail action.

### 4. Information should feel dense, not crowded

Reduce decorative padding. Increase structural clarity.

### 5. Make references effortless

Task IDs, project IDs, snapshot IDs, labels, and status references should be easy to scan, copy, and reuse.

### 6. Show truth explicitly

Do not hide auth failures, empty states, filter scope, or missing data.

## Global Application Shell

### Primary structure

- Left sidebar navigation
- Persistent top utility bar
- Main content region
- Optional right-side contextual panel

### Sidebar navigation

Order:

1. Home
2. Projects
3. Tasks
4. Sessions
5. Activity

Notes:

- `Board` should not be a top-level identity separate from tasks.
- The `Tasks` area should contain view switching for board, list, and detail-oriented flows.
- Navigation should support collapse-to-icons on narrower desktop widths.

### Top utility bar

Contains:

- global search entry
- current project scope indicator
- quick create button
- current user / auth state

Optional later:

- command palette trigger
- keyboard shortcut hint

### Right-side contextual panel

Use for:

- quick task detail preview
- quick comment thread
- activity item inspection
- project metadata peek

Do not use it for:

- long-form task editing that requires deep focus
- multi-step creation flows with many required fields

## Visual System

### Color

Base direction:

- off-black / graphite background for shell surfaces
- cooler white and slate tones for content surfaces
- one cyan-blue accent for focus, active state, links, and primary actions

Intent:

- feel technical and calm
- reduce gaming-dashboard energy
- preserve long-session readability

### Surface model

Use three surface levels:

1. App shell surface
2. Main workspace surface
3. Raised interactive surface

Raised surfaces should be used sparingly. Not every module should become a heavy card.

### Typography

- modern sans serif only
- compact heading scale
- strong tabular treatment for IDs, counts, timestamps, and metadata
- avoid giant hero headlines inside application views

### Spacing

- desktop default: compact but clean
- use tighter vertical rhythm in tables, timelines, and board cards
- reserve larger spacing only for page boundaries and section transitions

### Corners and borders

- moderate corner radius
- thin separators and structural borders
- avoid soft blob-like components

### Icons

- simple line icons
- consistent 16px and 20px sizes
- use icons to aid scanning, not decoration

## Interaction Model

### View switching

Within `Tasks`, support segmented switching:

- Board
- List
- Mine or Assigned

This switch belongs inside the Tasks workspace header, not the global shell.

### Selection model

- selecting a task should optionally open a right-side preview panel
- opening full detail should be a deliberate deeper action
- preserve list/board context when moving between preview and full detail

### Filters

Use a consistent filter pattern across Tasks and Activity:

- filter bar near the top of the workspace
- active filters shown as removable chips
- explicit `NOT` state visible on each filter chip or control
- clear-all action always visible when filters are active

### Empty states

Every empty state should explain:

- what scope is being viewed
- whether filters caused the empty result
- the most likely next action

### Errors and auth states

- show inline failure banners, not silent blank content
- separate auth problems from generic fetch failures
- preserve already loaded content where safe during refresh failures

## Page Redesign

## 1. Home

### Purpose

- orient the user at the start of a session
- highlight active work, not abstract analytics

### Replace current approach

Remove the oversized hero and equal-weight dashboard cards.

### New structure

1. Current scope strip
2. Active work summary
3. My open tasks
4. Recent activity
5. Recent sessions

### Content behavior

- the first screen should answer: what needs attention now?
- counts are secondary to actionable lists
- show small summary metrics only as supporting context

### Key components

- scope summary bar
- compact task list with status and priority
- recent activity feed with task/project jump links
- recent session snapshots with download affordance

## 2. Projects

### Purpose

- provide a scannable project index
- make project switching fast

### Structure

- toolbar with search and sort
- project table or dense list, not oversized cards by default
- optional summary tiles for a small number of pinned projects

### Row contents

- project name
- slug
- task counts by major status
- updated time
- concise description

### Interaction

- clicking a project sets active scope and enters Tasks view
- row quick actions can include copy ID and open project detail

## 3. Tasks Workspace

This is the center of the product.

### Tasks workspace header

Contains:

- page title
- current project scope
- board/list switch
- filter button or visible filter row
- quick create task button

### 3A. Board view

#### Direction

- use compact horizontal columns
- reduce card height significantly from the current mock
- keep card metadata scannable

#### Card contents

- task title
- copyable task ID
- priority marker
- status
- up to two labels before overflow
- assignee
- updated time

#### Interaction

- click opens preview panel
- secondary action opens full detail
- drag-and-drop may come later, but initial layout should not depend on it

#### Board rules

- sticky column headers
- horizontal scroll on smaller desktop widths
- dense enough to compare multiple tasks without fatigue

### 3B. List view

#### Direction

- default table-like dense layout
- optimized for scanning, sorting, bulk reading, and future bulk actions

#### Columns

- ID
- Title
- Status
- Priority
- Labels
- Assignee
- Updated

#### Interaction

- row click opens preview panel
- ID is directly copyable
- sorting should be available for core columns

### 3C. Task detail

#### Structure

1. Task header
2. Description and metadata
3. Parent and subtask structure
4. Comments
5. Activity timeline

#### Header contents

- title
- task ID copy action
- status selector
- priority selector
- assignee selector
- labels
- primary edit actions

#### Important design choice

Task structure must feel explicit.

Parent and subtask relationships are not side trivia. They are core product behavior and should be visually clear near the top of the detail view.

#### Comments and activity

- comments and activity should be visually distinct
- comments are conversational
- activity is audit-oriented metadata

## 4. Sessions

### Purpose

- act as a clean snapshot registry for exported OpenCode sessions

### Direction

- use a document-registry mental model, not a task board mental model
- prioritize upload, registration, metadata scan, and download

### Structure

1. header with upload action
2. sessions table or dense registry list
3. optional metadata side panel

### Registry columns

- Title
- Snapshot ID
- Description
- Artifact Name
- Artifact Path
- Updated

### Interaction

- upload creates or prepares a snapshot record
- download is a direct, visible action
- long paths should truncate visually but remain copyable
- `s3://...` and filesystem paths should be visually distinguished

## 5. Activity

### Purpose

- provide a global audit-friendly timeline with strong filtering

### Direction

- timeline plus structured filters
- emphasize precision and traceability over visual drama

### Structure

1. filter bar
2. active filter chips
3. sort control
4. timeline list

### Filter model

Must support:

- project
- task
- label
- combined filters
- `NOT`
- ascending and descending order

### Interaction details

- each timeline row should make project and task context obvious
- labels should be clickable into filter state
- filter state should remain visible even while scrolling

## Responsive Behavior

### Desktop

- primary design target
- full sidebar visible
- right-side preview panel enabled

### Tablet

- sidebar can collapse
- preview panel may become overlay drawer
- tables should preserve critical columns first

### Mobile

- navigation becomes drawer
- Tasks defaults to list over board
- detail is full-screen, not side-by-side
- filter controls may collapse into a sheet

## Interaction Patterns To Standardize

Use the same interaction language across the app for:

- copy ID
- open preview
- open full detail
- add filter
- negate filter
- clear filters
- download artifact
- show error
- show empty state

These should not be reinvented page by page.

## Component Priorities

The first reusable component set should focus on:

1. app shell
2. sidebar nav
3. workspace header
4. filter bar
5. chip and badge system
6. dense table
7. task card
8. right-side detail panel
9. activity timeline row
10. empty/error state blocks

## Explicit Design Rejections

Do not continue with:

- oversized landing-page hero patterns inside the app
- top-banner tab navigation as the main shell
- equally weighted large cards for every content block
- heavy gradient-first visual treatment
- decorative dashboard metrics dominating actionable work
- visually noisy dark theme patterns that reduce scan speed

## Implementation Notes

### Frontend architecture implication

The current single-file mock should not be evolved directly into production UI.

The frontend should be reorganized around:

- app shell
- view-level route components
- shared workspace components
- shared filter components
- shared metadata and state badge components

### Suggested first implementation sequence

1. app shell and sidebar
2. Tasks list view
3. task preview panel
4. task detail page
5. Activity page filter system
6. Sessions registry page
7. Home page refinement
8. Board view

This order prioritizes the most structurally important workflows first.
