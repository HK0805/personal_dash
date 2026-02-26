package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const requestTimeout = 8 * time.Second

type server struct {
	db        *sql.DB
	templates *template.Template
}

type dashboardCategory struct {
	ID    string
	Name  string
	Links []dashboardLink
}

type dashboardLink struct {
	ID         string
	CategoryID string
	Name       string
	URL        string
}

type dashboardData struct {
	Categories []dashboardCategory
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
	mux.HandleFunc("/actions/categories/create", s.handleCreateCategory)
	mux.HandleFunc("/actions/categories/", s.handleCategoryActions)
	mux.HandleFunc("/actions/links/create", s.handleCreateLink)
	mux.HandleFunc("/actions/links/", s.handleLinkActions)

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
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS categories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		);`,
		`CREATE TABLE IF NOT EXISTS links (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			category_id INTEGER NOT NULL,
			FOREIGN KEY(category_id) REFERENCES categories(id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_links_category ON links(category_id);`,
		`CREATE INDEX IF NOT EXISTS idx_links_name_category ON links(name, category_id);`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
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
	s.renderDashboard(w)
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
	if name == "" {
		http.Error(w, "category name is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	_, err := s.db.ExecContext(ctx, `INSERT INTO categories(name) VALUES(?)`, name)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			http.Error(w, "category already exists", http.StatusConflict)
			return
		}
		http.Error(w, "failed to create category", http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w)
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

	categoryID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
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
		http.Error(w, "failed to delete category links", http.StatusInternalServerError)
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

	s.renderDashboard(w)
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
	categoryIDText := strings.TrimSpace(r.FormValue("category_id"))

	if name == "" || url == "" || categoryIDText == "" {
		http.Error(w, "name, url, and category are required", http.StatusBadRequest)
		return
	}

	categoryID, err := strconv.ParseInt(categoryIDText, 10, 64)
	if err != nil {
		http.Error(w, "invalid category id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	_, err = s.db.ExecContext(ctx, `INSERT INTO links(name, url, category_id) VALUES(?, ?, ?)`, name, url, categoryID)
	if err != nil {
		http.Error(w, "failed to create link", http.StatusInternalServerError)
		return
	}

	s.renderDashboard(w)
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

	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
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
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if _, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, id); err != nil {
		http.Error(w, "failed to delete link", http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w)
}

func (s *server) handleUpdateLink(w http.ResponseWriter, r *http.Request, id int64) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	url := strings.TrimSpace(r.FormValue("url"))
	categoryIDText := strings.TrimSpace(r.FormValue("category_id"))

	if name == "" || url == "" || categoryIDText == "" {
		http.Error(w, "name, url, and category are required", http.StatusBadRequest)
		return
	}

	categoryID, err := strconv.ParseInt(categoryIDText, 10, 64)
	if err != nil {
		http.Error(w, "invalid category id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	_, err = s.db.ExecContext(ctx, `UPDATE links SET name = ?, url = ?, category_id = ? WHERE id = ?`, name, url, categoryID, id)
	if err != nil {
		http.Error(w, "failed to update link", http.StatusInternalServerError)
		return
	}

	s.renderDashboard(w)
}

func (s *server) renderDashboard(w http.ResponseWriter) {
	data, err := s.getDashboardData(context.Background())
	if err != nil {
		http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		http.Error(w, "failed to render template", http.StatusInternalServerError)
	}
}

func (s *server) getDashboardData(parent context.Context) (dashboardData, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()

	categoryRows, err := s.db.QueryContext(ctx, `SELECT id, name FROM categories`)
	if err != nil {
		return dashboardData{}, err
	}
	defer categoryRows.Close()

	categories := make([]dashboardCategory, 0)
	categoryMap := make(map[int64]*dashboardCategory)
	for categoryRows.Next() {
		var id int64
		var name string
		if err := categoryRows.Scan(&id, &name); err != nil {
			return dashboardData{}, err
		}
		item := dashboardCategory{ID: strconv.FormatInt(id, 10), Name: name, Links: []dashboardLink{}}
		categories = append(categories, item)
		categoryMap[id] = &categories[len(categories)-1]
	}
	if err := categoryRows.Err(); err != nil {
		return dashboardData{}, err
	}

	linkRows, err := s.db.QueryContext(ctx, `SELECT id, name, url, category_id FROM links`)
	if err != nil {
		return dashboardData{}, err
	}
	defer linkRows.Close()

	for linkRows.Next() {
		var id int64
		var name string
		var url string
		var categoryID int64
		if err := linkRows.Scan(&id, &name, &url, &categoryID); err != nil {
			return dashboardData{}, err
		}
		if parentCategory, ok := categoryMap[categoryID]; ok {
			parentCategory.Links = append(parentCategory.Links, dashboardLink{
				ID:         strconv.FormatInt(id, 10),
				CategoryID: strconv.FormatInt(categoryID, 10),
				Name:       name,
				URL:        url,
			})
		}
	}
	if err := linkRows.Err(); err != nil {
		return dashboardData{}, err
	}

	sort.Slice(categories, func(i, j int) bool {
		return strings.ToLower(categories[i].Name) < strings.ToLower(categories[j].Name)
	})
	for i := range categories {
		sort.Slice(categories[i].Links, func(a, b int) bool {
			return strings.ToLower(categories[i].Links[a].Name) < strings.ToLower(categories[i].Links[b].Name)
		})
	}

	return dashboardData{Categories: categories}, nil
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
