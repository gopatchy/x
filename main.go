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
}

func NewShortLinks() (*ShortLinks, error) {
	tmpl := template.New("index.html")

	tmpl, err := tmpl.ParseFiles("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("static/index.html: %w", err)
	}

	sl := &ShortLinks{
		tmpl: tmpl,
		mux:  http.NewServeMux(),
	}

	sl.mux.HandleFunc("/", sl.serveRoot)

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

	sl, err := NewShortLinks()
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
