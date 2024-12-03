package main

import (
	"database/sql"
	"encoding/json"
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
	help *template.Template
	mux  *http.ServeMux
	db   *sql.DB
	r    *rand.Rand
	oai  *oaiClient

	domainAliases   map[string]string
	writableDomains map[string]bool
}

type setResponse struct {
	Short  string `json:"short"`
	Domain string `json:"domain"`
}

type suggestResponse struct {
	Shorts []string `json:"shorts"`
	Domain string   `json:"domain"`
}

func NewShortLinks(db *sql.DB, domainAliases map[string]string, writableDomains map[string]bool) (*ShortLinks, error) {
	tmpl := template.New("index.html")

	tmpl, err := tmpl.ParseFiles("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("static/index.html: %w", err)
	}

	help, err := template.New("help.html").ParseFiles("static/help.html")
	if err != nil {
		return nil, fmt.Errorf("static/help.html: %w", err)
	}

	oai, err := newOAIClientFromEnv()
	if err != nil {
		return nil, fmt.Errorf("newOAIClientFromEnv: %w", err)
	}

	sl := &ShortLinks{
		tmpl: tmpl,
		help: help,
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
	err := sl.parseForm(r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "parse form: %s", err)
		return
	}

	if sl.isWritable(r.Host) {
		sl.serveRootWithPath(w, r, "")
		return
	}

	parts := strings.SplitN(r.Host, ".", 2)
	if len(parts) != 2 {
		sl.serveRootWithPath(w, r, "")
		return
	}

	long, err := sl.getLong(parts[0], sl.getDomain(parts[1]))
	if err != nil {
		sl.serveRootWithPath(w, r, "")
		return
	}

	http.Redirect(w, r, long, http.StatusTemporaryRedirect)
}

func (sl *ShortLinks) serveRootWithPath(w http.ResponseWriter, r *http.Request, path string) {
	err := sl.parseForm(r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "parse form: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
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
	log.Printf("%s %s %s %s %s %s", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL, r.Form)

	short := r.PathValue("short")

	if sl.isWritable(r.Host) && short == "_help" {
		sl.serveHelp(w, r)
		return
	}

	long, err := sl.getLong(short, sl.getDomain(r.Host))
	if err != nil {
		sl.serveRootWithPath(w, r, short)
		return
	}

	http.Redirect(w, r, long, http.StatusTemporaryRedirect)
}

func (sl *ShortLinks) serveSet(w http.ResponseWriter, r *http.Request) {
	err := sl.parseForm(r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "parse form: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
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
		Short:  short,
		Domain: sl.getDomain(r.Host),
	})
}

func (sl *ShortLinks) serveSuggest(w http.ResponseWriter, r *http.Request) {
	err := sl.parseForm(r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "parse form: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
		return
	}

	if !r.Form.Has("shorts") {
		sendError(w, http.StatusBadRequest, "shorts= param required")
		return
	}

	user := strings.Join(r.Form["shorts"], "\n")

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
		Domain: sl.getDomain(r.Host),
	})
}

func (sl *ShortLinks) serveHelp(w http.ResponseWriter, r *http.Request) {
	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
		return
	}

	err := sl.help.Execute(w, map[string]any{
		"host": r.Host,
	})
	if err != nil {
		sendError(w, http.StatusInternalServerError, "error executing template: %s", err)
		return
	}
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

func (sl *ShortLinks) getLong(short, domain string) (string, error) {
	var long string
	err := sl.db.QueryRow("SELECT long FROM links WHERE short = $1 AND domain = $2", short, domain).Scan(&long)
	if err != nil {
		return "", err
	}

	return long, nil
}

func (sl *ShortLinks) parseForm(r *http.Request) error {
	defer r.Body.Close()

	err := r.ParseForm()
	if err != nil {
		return err
	}

	if r.Header.Get("Content-Type") == "application/json" {
		dec := json.NewDecoder(r.Body)
		js := map[string]any{}
		err := dec.Decode(&js)
		if err != nil {
			return err
		}

		for k, v := range js {
			switch v := v.(type) {
			case []any:
				for _, s := range v {
					r.Form.Add(k, fmt.Sprintf("%v", s))
				}

			default:
				log.Printf("unknown type: %T", v)
				r.Form.Set(k, fmt.Sprintf("%v", v))
			}
		}
	}

	sl.logRequest(r)

	return nil
}

func (sl *ShortLinks) logRequest(r *http.Request) {
	log.Printf("%s %s %s %s %s %#v", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL, r.Form)
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
