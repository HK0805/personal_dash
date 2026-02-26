# personal_dash

Personal link dashboard for your browser new-tab page.

## Stack
- Astro (UI shell)
- HTMX + Alpine.js (interactivity)
- Go (`net/http`) API
- SQLite (local database)

## Prerequisites
- Node.js 18+ (`npm`)
- Go 1.22+

## Features
- Links grouped by category
- Create/delete categories
- Create/edit/delete links
- Fast partial refreshes using HTMX

## Project structure
- `backend/`: Go API + SQLite integration + HTML partial templates
- `src/pages/index.astro`: Astro new-tab page shell
- `src/styles/global.css`: Minimal UI styles

## 1) Configure backend env
Copy `backend/.env.example` to `backend/.env`.

Example `backend/.env`:

```env
SQLITE_PATH=./data/personal_dash.db
PORT=8080
```

## 2) Install frontend deps
```bash
cd /Users/dhamodharans/personal_dash
npm install
```

## 3) Run the full app (recommended)
```bash
cd /Users/dhamodharans/personal_dash
./scripts/dev.sh
```

This starts:
- Backend API on `http://localhost:8080`
- Astro frontend on `http://localhost:4321`

## 4) Run manually (optional)
```bash
cd backend
export $(grep -v '^#' .env | xargs)
go mod tidy
go run .
```

API will be available at `http://localhost:8080`.

```bash
cd /Users/dhamodharans/personal_dash
npm install
npm run dev
```

Frontend is available at `http://localhost:4321` and proxies `/backend/*` requests to Go API.

## Use as browser new tab
Set your browser's new-tab URL to:

`http://localhost:4321`

(If your browser requires an extension for custom new-tab URLs, use one like "Custom New Tab URL".)
