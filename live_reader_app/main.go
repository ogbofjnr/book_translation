package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultAddr  = ":8080"
	defaultModel = "deepseek-chat"
	deepseekURL  = "https://api.deepseek.com/chat/completions"
)

type app struct {
	db          *sql.DB
	booksDir    string
	apiKey      string
	model       string
	httpClient  *http.Client
	staticDir   string
	defaultUser string
}

type book struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	FileName    string    `json:"file_name"`
	Title       string    `json:"title,omitempty"`
	Author      string    `json:"author,omitempty"`
	DisplayName string    `json:"display_name"`
	StoredPath  string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

type annotateRequest struct {
	Text           string `json:"text"`
	TargetLanguage string `json:"target_language"`
	Threshold      string `json:"threshold"`
	Delimiter      string `json:"delimiter,omitempty"`
}

type annotateResponse struct {
	AnnotatedText string `json:"annotated_text"`
}

type deepseekRequest struct {
	Model       string            `json:"model"`
	Messages    []deepseekMessage `json:"messages"`
	Temperature float64           `json:"temperature"`
}

type deepseekMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type deepseekResponse struct {
	Choices []struct {
		Message deepseekMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func main() {
	_ = loadDotEnv(".env")
	_ = loadDotEnv("../.env")

	booksDir := "storage/books"
	if err := os.MkdirAll(booksDir, 0o755); err != nil {
		log.Fatalf("create books dir: %v", err)
	}
	if err := os.MkdirAll("storage", 0o755); err != nil {
		log.Fatalf("create storage dir: %v", err)
	}

	db, err := sql.Open("sqlite3", "storage/app.db")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		log.Fatalf("init db: %v", err)
	}

	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = defaultModel
	}

	a := &app{
		db:          db,
		booksDir:    booksDir,
		apiKey:      apiKey,
		model:       model,
		httpClient:  &http.Client{Timeout: 90 * time.Second},
		staticDir:   "web",
		defaultUser: "local-user",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/books", a.handleBooks)
	mux.HandleFunc("/api/books/upload", a.handleUpload)
	mux.HandleFunc("/api/books/", a.handleBookRoute)
	mux.HandleFunc("/api/annotate", a.handleAnnotate)
	mux.Handle("/", http.FileServer(http.Dir(a.staticDir)))

	log.Printf("listening on http://localhost%s", defaultAddr)
	if err := http.ListenAndServe(defaultAddr, logRequest(mux)); err != nil {
		log.Fatal(err)
	}
}

func initDB(db *sql.DB) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL;`,
		`CREATE TABLE IF NOT EXISTS books (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			file_name TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			author TEXT NOT NULL DEFAULT '',
			stored_path TEXT NOT NULL,
			created_at DATETIME NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_books_user_created ON books(user_id, created_at DESC);`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	// Backward-compatible migration for existing db files.
	_, _ = db.Exec(`ALTER TABLE books ADD COLUMN title TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE books ADD COLUMN author TEXT NOT NULL DEFAULT ''`)
	return nil
}

func (a *app) handleBooks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := a.userFromRequest(r)

	rows, err := a.db.Query(`SELECT id, user_id, file_name, title, author, stored_path, created_at FROM books WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []book
	for rows.Next() {
		var b book
		if err := rows.Scan(&b.ID, &b.UserID, &b.FileName, &b.Title, &b.Author, &b.StoredPath, &b.CreatedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b.DisplayName = chooseDisplayName(b)
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := a.userFromRequest(r)
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("book")
	if err != nil {
		http.Error(w, "missing file 'book'", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if !strings.EqualFold(filepath.Ext(header.Filename), ".epub") {
		http.Error(w, "only .epub is supported", http.StatusBadRequest)
		return
	}

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	stored := filepath.Join(a.booksDir, id+".epub")
	if err := saveUploadedFile(file, stored); err != nil {
		http.Error(w, "save file failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	title, author := extractEPUBMetadata(stored)

	created := time.Now().UTC()
	_, err = a.db.Exec(`INSERT INTO books(id, user_id, file_name, title, author, stored_path, created_at) VALUES(?,?,?,?,?,?,?)`,
		id, userID, header.Filename, title, author, stored, created)
	if err != nil {
		http.Error(w, "insert db failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, book{
		ID:          id,
		UserID:      userID,
		FileName:    header.Filename,
		Title:       title,
		Author:      author,
		DisplayName: displayName(header.Filename, title, author),
		StoredPath:  stored,
		CreatedAt:   created,
	})
}

func (a *app) handleBookRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/books/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	userID := a.userFromRequest(r)

	var b book
	err := a.db.QueryRow(`SELECT id, user_id, file_name, title, author, stored_path, created_at FROM books WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&b.ID, &b.UserID, &b.FileName, &b.Title, &b.Author, &b.StoredPath, &b.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "download" {
		w.Header().Set("Content-Type", "application/epub+zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", b.FileName))
		http.ServeFile(w, r, b.StoredPath)
		return
	}

	if r.Method == http.MethodDelete && len(parts) == 1 {
		if _, err := a.db.Exec(`DELETE FROM books WHERE id = ? AND user_id = ?`, id, userID); err != nil {
			http.Error(w, "delete db failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = os.Remove(b.StoredPath)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func chooseDisplayName(b book) string {
	return displayName(b.FileName, b.Title, b.Author)
}

func displayName(fileName, title, author string) string {
	title = strings.TrimSpace(title)
	author = strings.TrimSpace(author)
	if title != "" && author != "" {
		return title + " — " + author
	}
	if title != "" {
		return title
	}
	base := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	if base == "" {
		return fileName
	}
	return base
}

func extractEPUBMetadata(epubPath string) (title, author string) {
	zr, err := zip.OpenReader(epubPath)
	if err != nil {
		return "", ""
	}
	defer zr.Close()

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}

	containerPath := "META-INF/container.xml"
	cf, ok := files[containerPath]
	if !ok {
		return "", ""
	}
	containerData, err := readZipFile(cf)
	if err != nil {
		return "", ""
	}
	opfRel := parseContainerOPFPath(containerData)
	if opfRel == "" {
		return "", ""
	}
	opfRel = path.Clean(strings.TrimPrefix(opfRel, "/"))
	opf, ok := files[opfRel]
	if !ok {
		return "", ""
	}
	opfData, err := readZipFile(opf)
	if err != nil {
		return "", ""
	}
	return parseOPFTitleAuthor(opfData)
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func parseContainerOPFPath(containerXML []byte) string {
	type rootFile struct {
		FullPath string `xml:"full-path,attr"`
	}
	type container struct {
		Rootfiles []rootFile `xml:"rootfiles>rootfile"`
	}
	var c container
	if err := xml.Unmarshal(containerXML, &c); err != nil {
		return ""
	}
	for _, rf := range c.Rootfiles {
		if strings.TrimSpace(rf.FullPath) != "" {
			return strings.TrimSpace(rf.FullPath)
		}
	}
	return ""
}

func parseOPFTitleAuthor(opf []byte) (string, string) {
	dec := xml.NewDecoder(bytes.NewReader(opf))
	inMetadata := false
	var title, author string

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if local == "metadata" {
				inMetadata = true
				continue
			}
			if !inMetadata {
				continue
			}
			if local == "title" && title == "" {
				var v string
				if err := dec.DecodeElement(&v, &t); err == nil {
					title = strings.TrimSpace(v)
				}
			}
			if (local == "creator" || local == "author") && author == "" {
				var v string
				if err := dec.DecodeElement(&v, &t); err == nil {
					author = strings.TrimSpace(v)
				}
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "metadata" {
				inMetadata = false
			}
		}
	}
	return title, author
}

func (a *app) handleAnnotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(a.apiKey) == "" {
		http.Error(w, "DEEPSEEK_API_KEY is not configured", http.StatusServiceUnavailable)
		return
	}

	var req annotateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	req.TargetLanguage = strings.TrimSpace(req.TargetLanguage)
	req.Threshold = strings.ToUpper(strings.TrimSpace(req.Threshold))
	if req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	if req.TargetLanguage == "" {
		req.TargetLanguage = "Russian"
	}
	if req.Threshold == "" {
		req.Threshold = "B2"
	}

	systemPrompt := fmt.Sprintf(
		`You are a careful literary editor.
Task: keep the original English text exactly, and only add %s translations in parentheses for words/phrases that are ABOVE CEFR %s level.
Rules:
1) Preserve all original words, punctuation, line breaks, spacing, and order.
2) Do NOT rewrite or paraphrase sentences.
3) Insert translations only where needed by context, as: difficult_word (one best %s meaning by context).
4) In each pair of parentheses, provide exactly one %s meaning (no synonyms).
5) Keep names and simple %s-or-lower words untouched.
6) Output ONLY the transformed text, with no explanations.`,
		req.TargetLanguage, req.Threshold, req.TargetLanguage, req.TargetLanguage, req.Threshold,
	)
	if strings.TrimSpace(req.Delimiter) != "" {
		systemPrompt += fmt.Sprintf(
			"\n7) If you see the delimiter token %q in the input, preserve it exactly as-is, same count, same order, no edits.",
			req.Delimiter,
		)
	}

	out, err := a.callDeepSeek(systemPrompt, req.Text)
	if err != nil {
		http.Error(w, "annotate failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, annotateResponse{AnnotatedText: out})
}

func (a *app) callDeepSeek(systemPrompt, text string) (string, error) {
	body := deepseekRequest{
		Model: a.model,
		Messages: []deepseekMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: "Process this text:\n\n" + text},
		},
		Temperature: 0.1,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, deepseekURL, strings.NewReader(string(data)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(respData))
	}

	var parsed deepseekResponse
	if err := json.Unmarshal(respData, &parsed); err != nil {
		return "", err
	}
	if parsed.Error != nil {
		return "", errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("empty response choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

func saveUploadedFile(src multipart.File, destPath string) error {
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

func (a *app) userFromRequest(r *http.Request) string {
	userID := strings.TrimSpace(r.URL.Query().Get("user"))
	if userID == "" {
		userID = strings.TrimSpace(r.Header.Get("X-User-ID"))
	}
	if userID == "" {
		userID = a.defaultUser
	}
	return userID
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	return nil
}
