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
	list *template.Template
	mux  *http.ServeMux
	db   *sql.DB
	r    *rand.Rand
	oai  *oaiClient

	domainAliases   map[string]string
	writableDomains map[string]bool
	responseFormat  *oaiResponseFormat
}

type setResponse struct {
	Short  string `json:"short"`
	Domain string `json:"domain"`
	URL    string `json:"url"`
}

type suggestRequest struct {
	Shorts []string `json:"shorts,omitempty"`
	Title  string   `json:"title,omitempty"`
}

type suggestResponse struct {
	Shorts []string `json:"shorts"`
	Domain string   `json:"domain"`
}

type linkBase struct {
	Short     string `json:"short"`
	Long      string `json:"long"`
	Domain    string `json:"domain"`
	Generated bool   `json:"generated"`
	URL       string `json:"url"`
}

type link struct {
	linkBase

	History []linkHistory `json:"history"`
}

type linkHistory struct {
	linkBase

	Until time.Time `json:"until"`
}

func NewShortLinks(db *sql.DB, domainAliases map[string]string, writableDomains map[string]bool) (*ShortLinks, error) {
	funcMap := template.FuncMap{
		"lower": strings.ToLower,
		"join":  strings.Join,
	}

	tmpl, err := template.New("index.html").Funcs(funcMap).ParseFiles("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("static/index.html: %w", err)
	}

	help, err := template.New("help.html").Funcs(funcMap).ParseFiles("static/help.html")
	if err != nil {
		return nil, fmt.Errorf("static/help.html: %w", err)
	}

	list, err := template.New("list.html").Funcs(funcMap).ParseFiles("static/list.html")
	if err != nil {
		return nil, fmt.Errorf("static/list.html: %w", err)
	}

	oai, err := newOAIClientFromEnv()
	if err != nil {
		return nil, fmt.Errorf("newOAIClientFromEnv: %w", err)
	}

	sl := &ShortLinks{
		tmpl: tmpl,
		help: help,
		list: list,
		mux:  http.NewServeMux(),
		db:   db,
		r:    rand.New(rand.NewSource(uint64(time.Now().UnixNano()))),
		oai:  oai,

		domainAliases:   domainAliases,
		writableDomains: writableDomains,
		responseFormat: &oaiResponseFormat{
			Type: "json_schema",
			JSONSchema: map[string]any{
				"name":   "suggest_response",
				"strict": true,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"shorts": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "string",
							},
						},
					},
					"required":             []string{"shorts"},
					"additionalProperties": false,
				},
			},
		},
	}

	sl.mux.HandleFunc("GET /{$}", sl.serveRoot)
	sl.mux.HandleFunc("GET /_favicon.png", sl.serveFavicon)
	sl.mux.HandleFunc("GET /_help", sl.serveHelp)
	sl.mux.HandleFunc("GET /_list", sl.serveList)
	sl.mux.HandleFunc("GET /{short}", sl.serveShort)
	sl.mux.HandleFunc("POST /{$}", sl.serveSet)
	sl.mux.HandleFunc("QUERY /{$}", sl.serveSuggest)
	sl.mux.HandleFunc("OPTIONS /{$}", sl.serveOptions)

	return sl, nil
}

func (sl *ShortLinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sl.mux.ServeHTTP(w, r)
}

func (sl *ShortLinks) serveRoot(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	short := strings.ToLower(r.Form.Get("short"))

	if sl.isWritable(r.Host) {
		sl.serveRootWithShort(w, r, short)
		return
	}

	if sl.tryServeDomain(w, r) {
		return
	}

	if sl.tryServeFakeRoot(w, r) {
		return
	}

	sl.serveRootWithShort(w, r, short)
}

func (sl *ShortLinks) tryServeFakeRoot(w http.ResponseWriter, r *http.Request) bool {
	return sl.serveRedirect(w, r, "_root") == nil
}

func (sl *ShortLinks) tryServeDomain(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.SplitN(r.Host, ".", 2)
	if len(parts) != 2 {
		return false
	}

	short := strings.ToLower(parts[0])
	return sl.serveRedirect(w, r, short) == nil
}

func (sl *ShortLinks) serveRootWithShort(w http.ResponseWriter, r *http.Request, short string) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		err = sl.serveRedirect(w, r, "_404")
		if err != nil {
			sendError(w, http.StatusNotFound, "not found")
			return
		}

		return
	}

	err = sl.tmpl.Execute(w, map[string]any{
		"short": short,
		"host":  sl.getDomain(r.Host),
		"long":  r.Form.Get("long"),
		"title": r.Form.Get("title"),
	})
	if err != nil {
		sendError(w, http.StatusInternalServerError, "error executing template: %s", err)
		return
	}
}

func (sl *ShortLinks) serveShort(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	short := strings.ToLower(r.PathValue("short"))

	err = sl.serveRedirect(w, r, short)
	if err != nil {
		sl.serveRootWithShort(w, r, short)
		return
	}
}

func (sl *ShortLinks) serveRedirect(w http.ResponseWriter, r *http.Request, short string) error {
	long, err := sl.getLong(short, sl.getDomain(r.Host))
	if err != nil {
		return err
	}

	http.Redirect(w, r, long, http.StatusTemporaryRedirect)
	return nil
}

func (sl *ShortLinks) serveSet(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
		return
	}

	short := strings.ToLower(r.Form.Get("short"))
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
		URL:    fmt.Sprintf("https://%s/%s", sl.getDomain(r.Host), short),
	})
}

func (sl *ShortLinks) serveSuggest(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
		return
	}

	if !r.Form.Has("shorts") && !r.Form.Has("title") {
		sendError(w, http.StatusBadRequest, "shorts= or title= param required")
		return
	}

	in := suggestRequest{
		Shorts: r.Form["shorts"],
		Title:  r.Form.Get("title"),
	}

	out := &suggestResponse{}

	err = sl.oai.completeChat(
		"You are an assistant helping a user choose useful short names for a URL shortener. The request contains JSON object where the optional `shorts` key contains a list of recent names chosen by the user, with the most recent names first, and the optional `title` key contains a title for the URL. Respond with only a JSON object where the `shorts` key contains a list of possible suggestions for additional short names. In descending order of preference, suggestions should include: plural/singular variations, 2 and 3 letter abbreivations, conceptual variations, other variations that are likely to be useful. Your bar for suggestions should be relatively high; responding with a shorter list of high quality suggestions is preferred.",
		in,
		sl.responseFormat,
		out,
	)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "oai.completeChat: %s", err)
		return
	}

	send := &suggestResponse{
		Domain: sl.getDomain(r.Host),
	}

	for _, short := range out.Shorts {
		if short != "" {
			send.Shorts = append(send.Shorts, strings.ToLower(strings.TrimSpace(short)))
		}
	}

	sendJSON(w, send)
}

func (sl *ShortLinks) serveHelp(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
		return
	}

	err = sl.help.Execute(w, map[string]any{
		"writeHost": r.Host,
		"readHost":  sl.getDomain(r.Host),
	})
	if err != nil {
		sendError(w, http.StatusInternalServerError, "error executing template: %s", err)
		return
	}
}

func (sl *ShortLinks) serveOptions(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (sl *ShortLinks) genShort(domain string) (string, error) {
	for chars := 3; chars <= 10; chars++ {
		b := make([]byte, chars)

		for i := range b {
			b[i] = "0123456789abcdefghijklmnopqrstuvwxyz"[sl.r.Intn(62)]
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

func (sl *ShortLinks) serveFavicon(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	http.ServeFile(w, r, "static/favicon.png")
}

func (sl *ShortLinks) serveList(w http.ResponseWriter, r *http.Request) {
	err := sl.initRequest(w, r)
	if err != nil {
		sendError(w, http.StatusBadRequest, "init request: %s", err)
		return
	}

	if !sl.isWritable(r.Host) {
		sendError(w, http.StatusNotFound, "not found")
		return
	}

	rows, err := sl.db.Query(`
		SELECT
			short,
			long,
			domain,
			generated,
			CURRENT_TIMESTAMP as until,
			0 as is_history
		FROM links
		WHERE domain = $1

		UNION ALL

		SELECT
			short,
			long,
			domain,
			generated,
			until,
			1 as is_history
		FROM links_history
		WHERE domain = $1

		ORDER BY
			short ASC,
			is_history,
			until DESC
	`, sl.getDomain(r.Host))
	if err != nil {
		sendError(w, http.StatusInternalServerError, "select links: %s", err)
		return
	}

	defer rows.Close()

	links := []link{}

	for rows.Next() {
		link := link{}
		hist := linkHistory{}
		isHistory := false

		err := rows.Scan(&link.Short, &link.Long, &link.Domain, &link.Generated, &hist.Until, &isHistory)
		if err != nil {
			sendError(w, http.StatusInternalServerError, "scan link: %s", err)
			return
		}

		if !isHistory {
			link.URL = fmt.Sprintf("https://%s/%s", link.Domain, link.Short)
			links = append(links, link)
		} else {
			hist.linkBase = link.linkBase
			links[len(links)-1].History = append(links[len(links)-1].History, hist)
		}
	}

	err = sl.list.Execute(w, map[string]any{
		"links": links,
	})
	if err != nil {
		sendError(w, http.StatusInternalServerError, "error executing template: %s", err)
		return
	}
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

func (sl *ShortLinks) initRequest(w http.ResponseWriter, r *http.Request) error {
	log.Printf("%s %s %s %s %s %#v", r.RemoteAddr, r.Method, r.Host, sl.getDomain(r.Host), r.URL, r.Form)

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, QUERY, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")

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
				r.Form.Set(k, fmt.Sprintf("%v", v))
			}
		}
	}

	return nil
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
