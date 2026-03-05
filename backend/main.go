package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const requestTimeout = 8 * time.Second

var defaultPanels = []string{"Work", "Personal"}
var defaultCategories = []string{"Learning", "Entertainment", "Favorites", "Quick Links"}

type server struct {
	db        *sql.DB
	templates *template.Template
}

type dashboardPanel struct {
	ID   string
	Name string
}

type dashboardCategory struct {
	ID    string
	Name  string
	Links []dashboardLink
}

type dashboardLink struct {
	ID           string
	CategoryID   string
	CategoryName string
	Name         string
	URL          string
	Description  string
	LogoURL      string
}

type dashboardStats struct {
	TotalLinks      int
	Favorites       int
	RecentAdded     int
	TotalCategories int
}

type dashboardData struct {
	Panels      []dashboardPanel
	ActivePanel string
	Categories  []dashboardCategory
	QuickLinks  []dashboardLink
	Stats       dashboardStats
	SearchHint  string
	FormPanelID string
	PanelNotes  string
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.sqlitePath), 0o755); err != nil {
		log.Fatalf("create sqlite directory: %v", err)
	}

	db, err := sql.Open("sqlite", cfg.sqlitePath)
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping sqlite: %v", err)
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		log.Fatalf("enable foreign keys: %v", err)
	}
	if err := ensureSchema(db); err != nil {
		log.Fatalf("ensure schema: %v", err)
	}

	tpl, err := template.ParseFiles("templates/dashboard.html")
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	s := &server{db: db, templates: tpl}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/partials/dashboard", s.handleDashboard)
	mux.HandleFunc("/actions/panels/create", s.handleCreatePanel)
	mux.HandleFunc("/actions/panels/", s.handlePanelActions)
	mux.HandleFunc("/actions/categories/create", s.handleCreateCategory)
	mux.HandleFunc("/actions/categories/", s.handleCategoryActions)
	mux.HandleFunc("/actions/links/create", s.handleCreateLink)
	mux.HandleFunc("/actions/links/", s.handleLinkActions)
	mux.HandleFunc("/actions/reorder/categories", s.handleReorderCategories)
	mux.HandleFunc("/actions/reorder/links", s.handleReorderLinks)

	addr := fmt.Sprintf(":%s", cfg.port)
	log.Printf("api listening at http://localhost%s", addr)
	if err := http.ListenAndServe(addr, loggingMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

type config struct {
	sqlitePath string
	port       string
}

func loadConfig() (config, error) {
	sqlitePath := strings.TrimSpace(os.Getenv("SQLITE_PATH"))
	port := strings.TrimSpace(os.Getenv("PORT"))

	if sqlitePath == "" {
		sqlitePath = "./data/personal_dash.db"
	}
	if port == "" {
		port = "8080"
	}

	return config{sqlitePath: sqlitePath, port: port}, nil
}

func ensureSchema(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS panels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		position INTEGER NOT NULL DEFAULT 0
	);`); err != nil {
		return err
	}

	for idx, name := range defaultPanels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO panels(name, position)
			 VALUES(?, ?)
			 ON CONFLICT(name) DO UPDATE SET position = excluded.position`,
			name, idx,
		); err != nil {
			return err
		}
	}
	if err := addColumnIfMissing(ctx, tx, "panels", "notes", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	workPanelID, err := findPanelIDTx(ctx, tx, "Work")
	if err != nil {
		return err
	}

	if err := migrateCategoriesToPanels(ctx, tx, workPanelID); err != nil {
		return err
	}

	if err := addColumnIfMissing(ctx, tx, "links", "description", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, "links", "logo_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, "links", "custom_logo_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, "links", "position", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, "links", "created_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, "links", "updated_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := normalizeLinksForeignKey(ctx, tx); err != nil {
		return err
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE links SET position = id WHERE position = 0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE links SET created_at = ? WHERE created_at = 0`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE links SET updated_at = ? WHERE updated_at = 0`, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_categories_panel_position ON categories(panel_id, position)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_links_category_position ON links(category_id, position)`); err != nil {
		return err
	}

	if err := seedDefaultCategoriesTx(ctx, tx, workPanelID); err != nil {
		return err
	}

	return tx.Commit()
}

func migrateCategoriesToPanels(ctx context.Context, tx *sql.Tx, defaultPanelID int64) error {
	hasPanelID, err := columnExistsTx(ctx, tx, "categories", "panel_id")
	if err != nil {
		return err
	}

	if !hasPanelID {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE categories RENAME TO categories_legacy`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE categories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			panel_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			position INTEGER NOT NULL DEFAULT 0,
			UNIQUE(panel_id, name),
			FOREIGN KEY(panel_id) REFERENCES panels(id) ON DELETE CASCADE
		);`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO categories(id, panel_id, name, position)
			 SELECT id, ?, name, id
			 FROM categories_legacy
			 ORDER BY id ASC`,
			defaultPanelID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE categories_legacy`); err != nil {
			return err
		}
		return nil
	}

	if err := addColumnIfMissing(ctx, tx, "categories", "position", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE categories SET panel_id = ? WHERE panel_id IS NULL OR panel_id = 0`, defaultPanelID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE categories SET position = id WHERE position = 0`); err != nil {
		return err
	}
	return nil
}

func seedDefaultCategoriesTx(ctx context.Context, tx *sql.Tx, panelID int64) error {
	for idx, name := range defaultCategories {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO categories(panel_id, name, position) VALUES(?, ?, ?)`,
			panelID, name, idx+1,
		); err != nil {
			return err
		}
	}
	return nil
}

func normalizeLinksForeignKey(ctx context.Context, tx *sql.Tx) error {
	target, err := linksForeignKeyTargetTx(ctx, tx)
	if err != nil {
		return err
	}
	if target == "" || target == "categories" {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `ALTER TABLE links RENAME TO links_legacy`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE links (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		url TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		logo_url TEXT NOT NULL DEFAULT '',
		custom_logo_url TEXT NOT NULL DEFAULT '',
		category_id INTEGER NOT NULL,
		position INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY(category_id) REFERENCES categories(id) ON DELETE CASCADE
	);`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO links(
		id, name, url, description, logo_url, custom_logo_url, category_id, position, created_at, updated_at
	)
	SELECT
		id,
		name,
		url,
		COALESCE(description, ''),
		COALESCE(logo_url, ''),
		COALESCE(custom_logo_url, ''),
		category_id,
		CASE WHEN position = 0 THEN id ELSE position END,
		CASE WHEN created_at = 0 THEN CAST(strftime('%s','now') AS INTEGER) ELSE created_at END,
		CASE WHEN updated_at = 0 THEN CAST(strftime('%s','now') AS INTEGER) ELSE updated_at END
	FROM links_legacy`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE links_legacy`); err != nil {
		return err
	}
	return nil
}

func linksForeignKeyTargetTx(ctx context.Context, tx *sql.Tx) (string, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_list(links)`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id       int
			seq      int
			table    string
			fromCol  string
			toCol    string
			onUpdate string
			onDelete string
			match    string
		)
		if err := rows.Scan(&id, &seq, &table, &fromCol, &toCol, &onUpdate, &onDelete, &match); err != nil {
			return "", err
		}
		if strings.EqualFold(fromCol, "category_id") {
			return strings.ToLower(strings.TrimSpace(table)), nil
		}
	}
	return "", rows.Err()
}

func addColumnIfMissing(ctx context.Context, tx *sql.Tx, table string, column string, definition string) error {
	exists, err := columnExistsTx(ctx, tx, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func columnExistsTx(ctx context.Context, tx *sql.Tx, table string, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func findPanelIDTx(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM panels WHERE name = ? LIMIT 1`, name).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	activePanelID := parseInt64OrZero(strings.TrimSpace(r.URL.Query().Get("panel_id")))
	s.renderDashboard(w, activePanelID)
}

func (s *server) handleCreatePanel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "panel name is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	var nextPos int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM panels`).Scan(&nextPos); err != nil {
		http.Error(w, "failed to create panel", http.StatusInternalServerError)
		return
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO panels(name, position) VALUES(?, ?)`, name, nextPos)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			http.Error(w, "panel already exists", http.StatusConflict)
			return
		}
		http.Error(w, "failed to create panel", http.StatusInternalServerError)
		return
	}
	newID, _ := res.LastInsertId()
	s.renderDashboard(w, newID)
}

func (s *server) handlePanelActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/actions/panels/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	panelID := parseInt64OrZero(parts[0])
	if panelID == 0 {
		http.Error(w, "invalid panel id", http.StatusBadRequest)
		return
	}
	switch parts[1] {
	case "delete":
		s.handleDeletePanel(w, r, panelID)
	case "notes":
		s.handleUpdatePanelNotes(w, r, panelID)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleDeletePanel(w http.ResponseWriter, r *http.Request, panelID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM panels`).Scan(&count); err != nil {
		http.Error(w, "failed to delete panel", http.StatusInternalServerError)
		return
	}
	if count <= 1 {
		http.Error(w, "at least one panel is required", http.StatusBadRequest)
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "failed to delete panel", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	catRows, err := tx.QueryContext(ctx, `SELECT id FROM categories WHERE panel_id = ?`, panelID)
	if err != nil {
		http.Error(w, "failed to delete panel", http.StatusInternalServerError)
		return
	}
	catIDs := make([]int64, 0, 16)
	for catRows.Next() {
		var id int64
		if err := catRows.Scan(&id); err != nil {
			catRows.Close()
			http.Error(w, "failed to delete panel", http.StatusInternalServerError)
			return
		}
		catIDs = append(catIDs, id)
	}
	catRows.Close()

	for _, id := range catIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE category_id = ?`, id); err != nil {
			http.Error(w, "failed to delete panel", http.StatusInternalServerError)
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM categories WHERE panel_id = ?`, panelID); err != nil {
		http.Error(w, "failed to delete panel", http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM panels WHERE id = ?`, panelID); err != nil {
		http.Error(w, "failed to delete panel", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed to delete panel", http.StatusInternalServerError)
		return
	}

	s.renderDashboard(w, 0)
}

func (s *server) handleUpdatePanelNotes(w http.ResponseWriter, r *http.Request, panelID int64) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	notes := strings.TrimSpace(r.FormValue("notes"))
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE panels SET notes = ? WHERE id = ?`, notes, panelID); err != nil {
		http.Error(w, "failed to save notes", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	activePanelID := parseInt64OrZero(r.FormValue("active_panel_id"))
	if name == "" {
		http.Error(w, "category name is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	activePanelID, err := s.resolvePanelID(ctx, activePanelID)
	if err != nil {
		http.Error(w, "panel not found", http.StatusBadRequest)
		return
	}

	var nextPos int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM categories WHERE panel_id = ?`, activePanelID).Scan(&nextPos); err != nil {
		http.Error(w, "failed to create category", http.StatusInternalServerError)
		return
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO categories(panel_id, name, position) VALUES(?, ?, ?)`, activePanelID, name, nextPos)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			http.Error(w, "category already exists in this panel", http.StatusConflict)
			return
		}
		http.Error(w, "failed to create category", http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w, activePanelID)
}

func (s *server) handleCategoryActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/actions/categories/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[1] != "delete" {
		http.NotFound(w, r)
		return
	}
	categoryID := parseInt64OrZero(parts[0])
	activePanelID := parseInt64OrZero(r.FormValue("active_panel_id"))
	if categoryID == 0 {
		http.Error(w, "invalid category id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "failed to delete category", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE category_id = ?`, categoryID); err != nil {
		http.Error(w, "failed to delete category", http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM categories WHERE id = ?`, categoryID); err != nil {
		http.Error(w, "failed to delete category", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed to delete category", http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w, activePanelID)
}

func (s *server) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	url := strings.TrimSpace(r.FormValue("url"))
	description := strings.TrimSpace(r.FormValue("description"))
	categoryID := parseInt64OrZero(r.FormValue("category_id"))
	activePanelID := parseInt64OrZero(r.FormValue("active_panel_id"))

	if name == "" || url == "" || categoryID == 0 {
		http.Error(w, "name, url, and category are required", http.StatusBadRequest)
		return
	}
	if !isLikelyURL(url) {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	var nextPos int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM links WHERE category_id = ?`, categoryID).Scan(&nextPos); err != nil {
		http.Error(w, "failed to create link", http.StatusInternalServerError)
		return
	}
	now := time.Now().Unix()
	logo := derivedLogoURL(url)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO links(name, url, description, logo_url, category_id, position, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		name, url, description, logo, categoryID, nextPos, now, now,
	)
	if err != nil {
		http.Error(w, "failed to create link", http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w, activePanelID)
}

func (s *server) handleLinkActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/actions/links/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id := parseInt64OrZero(parts[0])
	if id == 0 {
		http.Error(w, "invalid link id", http.StatusBadRequest)
		return
	}
	switch parts[1] {
	case "delete":
		s.handleDeleteLink(w, r, id)
	case "update":
		s.handleUpdateLink(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleDeleteLink(w http.ResponseWriter, r *http.Request, id int64) {
	activePanelID := parseInt64OrZero(r.FormValue("active_panel_id"))
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, id); err != nil {
		http.Error(w, "failed to delete link", http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w, activePanelID)
}

func (s *server) handleUpdateLink(w http.ResponseWriter, r *http.Request, id int64) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	url := strings.TrimSpace(r.FormValue("url"))
	description := strings.TrimSpace(r.FormValue("description"))
	logoOverride := strings.TrimSpace(r.FormValue("custom_logo_url"))
	categoryID := parseInt64OrZero(r.FormValue("category_id"))
	activePanelID := parseInt64OrZero(r.FormValue("active_panel_id"))

	if name == "" || url == "" || categoryID == 0 {
		http.Error(w, "name, url, and category are required", http.StatusBadRequest)
		return
	}
	if !isLikelyURL(url) {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	logo := derivedLogoURL(url)
	if logoOverride != "" {
		logo = logoOverride
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE links
		 SET name = ?, url = ?, description = ?, logo_url = ?, custom_logo_url = ?, category_id = ?, updated_at = ?
		 WHERE id = ?`,
		name, url, description, logo, logoOverride, categoryID, now, id,
	)
	if err != nil {
		http.Error(w, "failed to update link", http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w, activePanelID)
}

func (s *server) handleReorderCategories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	panelID := parseInt64OrZero(r.FormValue("panel_id"))
	ordered := parseIDList(r.FormValue("ordered_ids"))
	if panelID == 0 {
		http.Error(w, "invalid panel id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "failed to reorder categories", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	for idx, id := range ordered {
		if _, err := tx.ExecContext(ctx, `UPDATE categories SET position = ? WHERE id = ? AND panel_id = ?`, idx, id, panelID); err != nil {
			http.Error(w, "failed to reorder categories", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed to reorder categories", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleReorderLinks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	categoryID := parseInt64OrZero(r.FormValue("category_id"))
	ordered := parseIDList(r.FormValue("ordered_ids"))
	if categoryID == 0 {
		http.Error(w, "invalid category id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "failed to reorder links", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for idx, id := range ordered {
		if _, err := tx.ExecContext(ctx, `UPDATE links SET category_id = ?, position = ?, updated_at = ? WHERE id = ?`, categoryID, idx, now, id); err != nil {
			http.Error(w, "failed to reorder links", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed to reorder links", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) renderDashboard(w http.ResponseWriter, requestedPanelID int64) {
	data, err := s.getDashboardData(context.Background(), requestedPanelID)
	if err != nil {
		http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		http.Error(w, "failed to render template", http.StatusInternalServerError)
	}
}

func (s *server) getDashboardData(parent context.Context, requestedPanelID int64) (dashboardData, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()

	panels, err := s.loadPanels(ctx)
	if err != nil {
		return dashboardData{}, err
	}
	if len(panels) == 0 {
		return dashboardData{}, errors.New("no panels available")
	}

	activePanelID := requestedPanelID
	if activePanelID == 0 {
		activePanelID = panels[0].ID
	}
	if !panelExists(panels, activePanelID) {
		activePanelID = panels[0].ID
	}
	panelNotes := ""
	if err := s.db.QueryRowContext(ctx, `SELECT notes FROM panels WHERE id = ?`, activePanelID).Scan(&panelNotes); err != nil {
		return dashboardData{}, err
	}

	categories, categoryMap, err := s.loadCategoriesForPanel(ctx, activePanelID)
	if err != nil {
		return dashboardData{}, err
	}

	allLinks := make([]dashboardLink, 0, 64)
	favoritesCount := 0

	rows, err := s.db.QueryContext(ctx,
		`SELECT l.id, l.name, l.url, l.description, l.logo_url, l.category_id
		 FROM links l
		 JOIN categories c ON c.id = l.category_id
		 WHERE c.panel_id = ?
		 ORDER BY l.position ASC, l.id ASC`,
		activePanelID,
	)
	if err != nil {
		return dashboardData{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var name, url, description, logo string
		var categoryID int64
		if err := rows.Scan(&id, &name, &url, &description, &logo, &categoryID); err != nil {
			return dashboardData{}, err
		}
		cat, ok := categoryMap[categoryID]
		if !ok {
			continue
		}
		item := dashboardLink{
			ID:           strconv.FormatInt(id, 10),
			CategoryID:   strconv.FormatInt(categoryID, 10),
			CategoryName: cat.Name,
			Name:         name,
			URL:          url,
			Description:  description,
			LogoURL:      logo,
		}
		cat.Links = append(cat.Links, item)
		allLinks = append(allLinks, item)
		if strings.EqualFold(cat.Name, "Favorites") {
			favoritesCount++
		}
	}
	if err := rows.Err(); err != nil {
		return dashboardData{}, err
	}

	recentAdded := len(allLinks)
	if recentAdded > 3 {
		recentAdded = 3
	}

	quickLinks := allLinks
	if len(quickLinks) > 5 {
		quickLinks = quickLinks[:5]
	}

	panelView := make([]dashboardPanel, 0, len(panels))
	for _, p := range panels {
		panelView = append(panelView, dashboardPanel{ID: strconv.FormatInt(p.ID, 10), Name: p.Name})
	}

	return dashboardData{
		Panels:      panelView,
		ActivePanel: strconv.FormatInt(activePanelID, 10),
		Categories:  categories,
		QuickLinks:  quickLinks,
		Stats: dashboardStats{
			TotalLinks:      len(allLinks),
			Favorites:       favoritesCount,
			RecentAdded:     recentAdded,
			TotalCategories: len(categories),
		},
		SearchHint:  fmt.Sprintf("Search links in %s...", findPanelName(panels, activePanelID)),
		FormPanelID: strconv.FormatInt(activePanelID, 10),
		PanelNotes:  panelNotes,
	}, nil
}

type panelRow struct {
	ID       int64
	Name     string
	Position int
}

func (s *server) loadPanels(ctx context.Context) ([]panelRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, position FROM panels ORDER BY position ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]panelRow, 0, 8)
	for rows.Next() {
		var row panelRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Position); err != nil {
			return nil, err
		}
		items = append(items, row)
	}
	return items, rows.Err()
}

func (s *server) loadCategoriesForPanel(ctx context.Context, panelID int64) ([]dashboardCategory, map[int64]*dashboardCategory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name FROM categories WHERE panel_id = ? ORDER BY position ASC, id ASC`,
		panelID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	categories := make([]dashboardCategory, 0, 16)
	catMap := make(map[int64]*dashboardCategory)
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, nil, err
		}
		item := dashboardCategory{ID: strconv.FormatInt(id, 10), Name: name, Links: []dashboardLink{}}
		categories = append(categories, item)
		catMap[id] = &categories[len(categories)-1]
	}
	return categories, catMap, rows.Err()
}

func panelExists(items []panelRow, id int64) bool {
	for _, p := range items {
		if p.ID == id {
			return true
		}
	}
	return false
}

func findPanelName(items []panelRow, id int64) string {
	for _, p := range items {
		if p.ID == id {
			return p.Name
		}
	}
	return "Panel"
}

func (s *server) resolvePanelID(ctx context.Context, requested int64) (int64, error) {
	if requested != 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM panels WHERE id = ?`, requested).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}

	var fallback int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM panels ORDER BY position ASC, id ASC LIMIT 1`).Scan(&fallback); err != nil {
		return 0, err
	}
	return fallback, nil
}

func parseInt64OrZero(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseIDList(raw string) []int64 {
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		if id := parseInt64OrZero(p); id != 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func isLikelyURL(url string) bool {
	url = strings.ToLower(strings.TrimSpace(url))
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

func derivedLogoURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	host := strings.Split(trimmed, "/")[0]
	if host == "" {
		return ""
	}
	return "https://www.google.com/s2/favicons?domain=" + host + "&sz=64"
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
