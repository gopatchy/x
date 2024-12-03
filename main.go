package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"golang.org/x/exp/rand"
)

type ShortLinks struct {
	tmpl *template.Template
	mux  *http.ServeMux
	db   *sql.DB
	r    *rand.Rand
	oai  *oaiClient

	domainAliases   map[string]string
	writableDomains map[string]bool
}

type setResponse struct {
	Short string `json:"short"`
}

type suggestResponse struct {
	Shorts []string `json:"shorts"`
}

func NewShortLinks(db *sql.DB, domainAliases map[string]string, writableDomains map[string]bool) (*ShortLinks, error) {
	tmpl := template.New("index.html")

	tmpl, err := tmpl.ParseFiles("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("static/index.html: %w", err)
	}

	oai, err := newOAIClientFromEnv()
	if err != nil {
		return nil, fmt.Errorf("newOAIClientFromEnv: %w", err)
	}

	sl := &ShortLinks{
		tmpl: tmpl,
		mux:  http.NewServeMux(),
		db:   db,
		r:    rand.New(rand.NewSource(uint64(time.Now().UnixNano()))),
		oai:  oai,

		domainAliases:   domainAliases,
		writableDomains: writableDomains,
	}

	sl.mux.HandleFunc("GET /{$}", sl.serveRoot)
	sl.mux.HandleFunc("GET /{short}", sl.serveShort)
	sl.mux.HandleFunc("POST /{$}", sl.serveSet)
	sl.mux.HandleFunc("QUERY /{$}", sl.serveSuggest)

	return sl, nil
}

func (sl *ShortLinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sl.mux.ServeHTTP(w, r)
}

func (sl *ShortLinks) serveRoot(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s %s %s %s", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL)

	sl.serveRootWithPath(w, r, "")
}

func (sl *ShortLinks) serveRootWithPath(w http.ResponseWriter, r *http.Request, path string) {
	err := r.ParseForm()
	if err != nil {
		sendError(w, http.StatusBadRequest, "Parse form: %s", err)
		return
	}
	log.Printf("%s %s %s %s %s", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL)

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusForbidden, "not writable")
		return
	}

	err = sl.tmpl.Execute(w, map[string]any{
		"path": path,
		"host": sl.getDomain(r.Host),
		"long": r.Form.Get("long"),
	})
	if err != nil {
		sendError(w, http.StatusInternalServerError, "error executing template: %s", err)
		return
	}
}

func (sl *ShortLinks) serveShort(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s %s %s %s", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL)

	short := r.PathValue("short")

	row := sl.db.QueryRow(`SELECT long FROM links WHERE short = $1 AND domain = $2`, short, sl.getDomain(r.Host))
	var long string
	err := row.Scan(&long)
	if err != nil {
		sl.serveRootWithPath(w, r, short)
		return
	}

	http.Redirect(w, r, long, http.StatusTemporaryRedirect)
}

func (sl *ShortLinks) serveSet(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		sendError(w, http.StatusBadRequest, "Parse form: %s", err)
		return
	}

	log.Printf("%s %s %s %s %s", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL)

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusForbidden, "not writable")
		return
	}

	short := r.Form.Get("short")
	generated := false

	if short == "" {
		short, err = sl.genShort(sl.getDomain(r.Host))
		if err != nil {
			sendError(w, http.StatusInternalServerError, "genShort: %s", err)
			return
		}

		generated = true
	}

	long := r.Form.Get("long")
	if long == "" {
		sendError(w, http.StatusBadRequest, "long= param required")
		return
	}

	_, err = sl.db.Exec(`SELECT update_link($1, $2, $3, $4);`, short, long, sl.getDomain(r.Host), generated)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "update_link: %s", err)
		return
	}

	sendJSON(w, setResponse{
		Short: short,
	})
}

func (sl *ShortLinks) serveSuggest(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		sendError(w, http.StatusBadRequest, "Parse form: %s", err)
		return
	}

	log.Printf("%s %s %s %s %s", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL)

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusForbidden, "not writable")
		return
	}

	if !r.Form.Has("short") {
		sendError(w, http.StatusBadRequest, "short= param required")
		return
	}

	user := strings.Join(r.Form["short"], "\n")

	comp, err := sl.oai.completeChat(
		"You are an assistant helping a user choose useful short names for a URL shortener. The request contains a list recents names chosen by the user, separated by newlines, with the most recent names first. Respond with only a list of possible suggestions for additional short names, separated by newlines. In descending order of preference, suggestions should include: plural/singular variations, 2 and 3 letter abbreivations, conceptual variations, other variations that are likely to be useful. Your bar for suggestions should be relatively high; responding with a shorter list of high quality suggestions is preferred.",
		user,
	)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "oai.completeChat: %s", err)
		return
	}

	shorts := []string{}
	for _, short := range strings.Split(comp, "\n") {
		if short != "" {
			shorts = append(shorts, strings.TrimSpace(short))
		}
	}

	sendJSON(w, suggestResponse{
		Shorts: shorts,
	})
}

func (sl *ShortLinks) genShort(domain string) (string, error) {
	for chars := 3; chars <= 10; chars++ {
		b := make([]byte, chars)

		for i := range b {
			b[i] = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"[sl.r.Intn(62)]
		}

		short := string(b)

		exists := false
		err := sl.db.QueryRow("SELECT EXISTS(SELECT 1 FROM links WHERE short = $1 AND domain = $2)", short, domain).Scan(&exists)
		if err != nil {
			return "", fmt.Errorf("check exists: %w", err)
		}

		if !exists {
			return short, nil
		}
	}

	return "", fmt.Errorf("no available short link found")
}

func (sl *ShortLinks) getDomain(host string) string {
	if alias, ok := sl.domainAliases[host]; ok {
		return alias
	}

	return host
}

func (sl *ShortLinks) isWritable(host string) bool {
	return sl.writableDomains[host]
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

	stmts := []string{
		`
    CREATE TABLE IF NOT EXISTS links (
        short VARCHAR(100) NOT NULL,
        long TEXT NOT NULL,
		domain VARCHAR(255) NOT NULL,
		generated BOOLEAN NOT NULL,
		PRIMARY KEY (short, domain)
    );
	`,

		`
	CREATE TABLE IF NOT EXISTS links_history (
		short VARCHAR(100),
		long TEXT NOT NULL,
		domain VARCHAR(255) NOT NULL,
		generated BOOLEAN NOT NULL,
		until TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
	);
	`,

		`
	CREATE OR REPLACE FUNCTION update_link(
		_short VARCHAR(100),
		_long TEXT,
		_domain VARCHAR(255),
		_generated BOOLEAN
	) RETURNS void AS $$
	DECLARE
		old RECORD;
	BEGIN
		SELECT * INTO old FROM links WHERE short = _short AND domain = _domain;

		IF old IS NOT NULL THEN
			INSERT INTO links_history (short, long, domain, generated)
			VALUES (old.short, old.long, old.domain, old.generated);

			UPDATE links
			SET long = _long, generated = _generated
			WHERE short = _short AND domain = _domain;
		ELSE
			INSERT INTO links (short, long, domain, generated)
			VALUES (_short, _long, _domain, _generated);
		END IF;
	END;
	$$ LANGUAGE plpgsql;
	`,
	}

	for _, stmt := range stmts {
		_, err := db.Exec(stmt)
		if err != nil {
			log.Fatalf("Failed to create tables & functions: %v", err)
		}
	}

	domainAliases, err := loadDomainAliases()
	if err != nil {
		log.Fatalf("Failed to load domain aliases: %v", err)
	}

	writableDomains, err := loadWritableDomains()
	if err != nil {
		log.Fatalf("Failed to load writable domains: %v", err)
	}

	sl, err := NewShortLinks(db, domainAliases, writableDomains)
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

func loadDomainAliases() (map[string]string, error) {
	ret := map[string]string{}

	s := os.Getenv("DOMAIN_ALIASES")
	if s == "" {
		return ret, nil
	}

	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid domain alias: %s", pair)
		}

		ret[parts[0]] = parts[1]
	}

	return ret, nil
}

func loadWritableDomains() (map[string]bool, error) {
	ret := map[string]bool{}

	s := os.Getenv("WRITABLE_DOMAINS")
	if s == "" {
		return ret, nil
	}

	for _, domain := range strings.Split(s, ",") {
		ret[domain] = true
	}

	return ret, nil
}
