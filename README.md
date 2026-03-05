# Life Panels Dashboard

A local-first personal link manager designed for browser new-tab usage.
It supports multiple life panels (Work, Personal, etc.), fast link organization, drag/drop workflows, and cross-tab syncing.

## What This Project Solves
Most bookmark tools are either too simple or too heavy. This project is a focused middle ground:
- quick capture and edit of useful links
- panel-based organization for different areas of life
- instant interactions without a heavy frontend framework runtime
- reliable local persistence via SQLite

## Core Features
- Multi-panel organization
  - Default panels: `Work`, `Personal`
  - Create and delete panels
  - Each panel has isolated categories, links, and notes
- Category and link management
  - Create/delete categories
  - Create/edit/delete links
  - Link metadata: `title`, `url`, `description`, `logo`
- Smart logo support
  - Auto-derives favicon URL using Google favicon endpoint
  - Optional custom logo URL override
- Drag and drop
  - Reorder categories within a panel
  - Reorder links inside a category
  - Move links across categories
- Search and filtering
  - Panel-scoped search by title, URL, and description
- Notes / scratchpad
  - Per-panel notes
  - Debounced autosave
  - Clear notes with confirmation
- Keyboard shortcuts
  - `1`–`9`: switch among first 9 panels
  - `/`: focus search
  - `n`: focus add-link input
  - Shortcuts are disabled while typing in form fields
- Cross-tab sync
  - BroadcastChannel (primary)
  - `storage` event fallback
  - Syncs panel/category/link/notes mutations
- Dynamic greeting + theme
  - Time-based greeting updates every minute
  - Day/night toggle with animation

## Tech Stack and Tools
- Frontend shell: **Astro**
- Interactivity: **HTMX** + **Alpine.js**
- Drag/drop: **SortableJS**
- Backend: **Go** (`net/http`, server-rendered partials)
- Database: **SQLite** (`modernc.org/sqlite` driver)
- Build/runtime: **npm**, **Go toolchain**

## Architecture (High Level)
- Server-rendered UI with partial updates
  - Astro serves page shell
  - Go renders dashboard HTML partials
  - HTMX posts actions and swaps `#dashboard` without full reload
- Local persistence
  - SQLite stores all app state
  - Schema migration runs on startup and is backward-safe
- Event-driven UX
  - Alpine handles local UI state (filters, edit toggles, greeting, theme)
  - SortableJS emits reorder events persisted through Go endpoints
- Cross-tab consistency
  - Mutation broadcasts trigger dashboard refresh in other tabs

## Data Model (Current)
- `panels`
  - `id`, `name`, `position`, `notes`
- `categories`
  - `id`, `panel_id`, `name`, `position`
- `links`
  - `id`, `name`, `url`, `description`, `logo_url`, `custom_logo_url`, `category_id`, `position`, `created_at`, `updated_at`

## Project Structure
- `backend/main.go`: API handlers, schema migration, business logic
- `backend/templates/dashboard.html`: server-rendered dashboard partial
- `src/pages/index.astro`: app shell + global scripts
- `src/styles/global.css`: styling and layout
- `scripts/dev.sh`: starts backend + frontend together

## Local Setup
### Prerequisites
- Node.js 18+
- Go 1.22+

### 1) Install dependencies
```bash
cd <project-root>
npm install
```

### 2) Configure backend env
```bash
cd backend
cp .env.example .env
```

Default `.env`:
```env
SQLITE_PATH=./data/personal_dash.db
PORT=8080
```

### 3) Run app (recommended)
```bash
cd <project-root>
./scripts/dev.sh
```

- Frontend: `http://localhost:4321`
- Backend: `http://localhost:8080`

## Build and Test
```bash
cd backend && go test ./...
cd .. && npm run build
```

## API Server
The backend is a Go `net/http` server.

- Default base URL: `http://localhost:8080`
- Health endpoint: `GET /health`
- Dashboard partial endpoint: `GET /partials/dashboard?panel_id=<id>`

### Main action APIs (HTMX form endpoints)
- Panels
  - `POST /actions/panels/create`
  - `POST /actions/panels/{panelId}/delete`
  - `POST /actions/panels/{panelId}/notes`
  - `POST /actions/panels/{panelId}/notes-clear`
- Categories
  - `POST /actions/categories/create`
  - `POST /actions/categories/{categoryId}/delete`
  - `POST /actions/reorder/categories`
- Links
  - `POST /actions/links/create`
  - `POST /actions/links/{linkId}/update`
  - `POST /actions/links/{linkId}/delete`
  - `POST /actions/reorder/links`

## Manual QA Checklist
- Create/switch/delete panels
- Add/edit/delete categories and links in the active panel
- Drag categories and links; reload and verify order persistence
- Move a link to another category via drag/drop
- Add notes, refresh, and clear notes
- Open two tabs; verify updates sync between tabs
- Verify keyboard shortcuts and that they do not trigger while typing

## New Tab Usage
Set browser new-tab URL to:
`http://localhost:4321`

If your browser blocks custom new-tab URLs, use a lightweight extension to point new tabs to localhost.
