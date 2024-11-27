package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"

	_ "github.com/lib/pq"
)

type ShortLinks struct {
	tmpl *template.Template
	mux  *http.ServeMux
	db   *sql.DB
}

type response struct {
	Short string `json:"short"`
}

func NewShortLinks(db *sql.DB) (*ShortLinks, error) {
	tmpl := template.New("index.html")

	tmpl, err := tmpl.ParseFiles("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("static/index.html: %w", err)
	}

	sl := &ShortLinks{
		tmpl: tmpl,
		mux:  http.NewServeMux(),
		db:   db,
	}

	sl.mux.HandleFunc("GET /{$}", sl.serveRoot)
	sl.mux.HandleFunc("GET /{short}", sl.serveShort)
	sl.mux.HandleFunc("POST /{$}", sl.serveSet)

	return sl, nil
}

func (sl *ShortLinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sl.mux.ServeHTTP(w, r)
}

func (sl *ShortLinks) serveRoot(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.RemoteAddr, r.URL.Path)

	err := sl.tmpl.Execute(w, nil)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "error executing template: %s", err)
		return
	}
}

func (sl *ShortLinks) serveShort(w http.ResponseWriter, r *http.Request) {
	sendError(w, http.StatusNotImplemented, "not implemented")
}

func (sl *ShortLinks) serveSet(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		sendError(w, http.StatusBadRequest, "Parse form: %s", err)
		return
	}

	log.Printf("%s %s %s", r.RemoteAddr, r.URL.Path, r.Form.Encode())

	short := r.Form.Get("short")

	long := r.Form.Get("long")
	if long == "" {
		sendError(w, http.StatusBadRequest, "long= param required")
		return
	}

	_, err = sl.db.Exec(`
		INSERT INTO links (short, long)
		VALUES ($1, $2)
		ON CONFLICT (short)
		DO UPDATE SET long = $2
	`, short, long)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "upsert: %s", err)
		return
	}

	sendJSON(w, response{
		Short: short,
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		log.Fatalf("please set PORT")
	}

	pgConn := os.Getenv("PGCONN")
	if pgConn == "" {
		log.Fatalf("please set PGCONN")
	}

	db, err := sql.Open("postgres", pgConn)
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS links (
    short VARCHAR(100) PRIMARY KEY,
    long VARCHAR(255) NOT NULL
);`)
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}

	sl, err := NewShortLinks(db)
	if err != nil {
		log.Fatalf("Failed to create shortlinks: %v", err)
	}

	http.Handle("/", sl)

	bind := fmt.Sprintf(":%s", port)
	log.Printf("listening on %s", bind)

	if err := http.ListenAndServe(bind, nil); err != nil {
		log.Fatalf("listen: %s", err)
	}
}
