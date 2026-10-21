package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/task"
)

//go:embed dash
var dashFS embed.FS

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func cmdDashboard(args []string) int {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	port := fs.Int("port", 7777, "listen on 127.0.0.1:`N` (0 = any free port)")
	noOpen := fs.Bool("no-open", false, "don't open a browser")
	var allow stringList
	fs.Var(&allow, "allow-host", "also accept this Host header, e.g. vps.tailnet.ts.net (repeatable)")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	r, err := openRepo()
	defer r.close()
	if err != nil || r.stErr != nil {
		fmt.Fprintf(os.Stderr, "cinema dashboard: %v\n", errors.Join(err, r.stErr))
		return exitBadArgs
	}
	// Loopback only (plan §9.1, §10.4); remote access goes through tailscale serve or ssh -L.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema dashboard: %v\n", err)
		return exitBadArgs
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d := newDash(ctx, r, actual, allow)
	srv := &http.Server{Handler: d.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/#t=%s", actual, d.token)
	fmt.Printf("cinema dashboard for %s\n  %s\n(the link carries a one-time token; Ctrl-C to stop)\n", r.root, url)
	if !*noOpen {
		openBrowser(url)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "cinema dashboard: %v\n", err)
		return exitInternal
	}
	d.wg.Wait() // runs started from the page finish their own cleanup
	return 0
}

type dashSrv struct {
	ctx   context.Context
	token string
	hosts map[string]bool
	port  int

	mu sync.Mutex // guards r: repo memos and cache aren't safe for concurrent use
	r  *repo

	smu      sync.Mutex
	sessions map[string]string // session id → CSRF token
	running  map[string]bool   // task IDs started from the page
	wg       sync.WaitGroup
}

func newDash(ctx context.Context, r *repo, port int, allow []string) *dashSrv {
	d := &dashSrv{ctx: ctx, token: randHex(32), r: r, port: port,
		sessions: map[string]string{}, running: map[string]bool{}, hosts: map[string]bool{}}
	p := strconv.Itoa(port)
	for _, h := range append([]string{"127.0.0.1", "localhost"}, allow...) {
		d.hosts[h+":"+p] = true
	}
	for _, h := range allow {
		d.hosts[h] = true // behind tailscale serve the Host has no port
	}
	return d
}

func (d *dashSrv) handler() http.Handler {
	static, _ := fs.Sub(dashFS, "dash")
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.FileServerFS(static))
	mux.Handle("GET /app.js", http.FileServerFS(static))
	mux.Handle("GET /app.css", http.FileServerFS(static))
	mux.HandleFunc("POST /api/session", d.session)
	mux.HandleFunc("GET /api/state", d.authed(d.state))
	mux.HandleFunc("GET /api/runs/{id}", d.authed(d.runDetail))
	mux.HandleFunc("GET /api/events", d.authed(d.events))
	mux.HandleFunc("POST /api/run", d.authed(d.startRun))
	return d.guard(mux)
}

// guard applies to every request: Host allowlist (blocks DNS rebinding),
// same-origin for anything that isn't a GET, and strict headers.
func (d *dashSrv) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !d.hosts[req.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if req.Method != http.MethodGet {
			o := req.Header.Get("Origin")
			if o != "http://"+req.Host && o != "https://"+req.Host {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, req)
	})
}

func (d *dashSrv) cookieName() string { return "cinema_session_" + strconv.Itoa(d.port) }

// session exchanges the one-time URL token for an HttpOnly cookie.
func (d *dashSrv) session(w http.ResponseWriter, req *http.Request) {
	var body struct{ Token string }
	if json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024)).Decode(&body) != nil ||
		subtle.ConstantTimeCompare([]byte(body.Token), []byte(d.token)) != 1 {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	id := randHex(32)
	d.smu.Lock()
	d.sessions[id] = randHex(32)
	d.smu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: d.cookieName(), Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: req.Header.Get("X-Forwarded-Proto") == "https"})
	w.WriteHeader(http.StatusNoContent)
}

func (d *dashSrv) authed(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		c, err := req.Cookie(d.cookieName())
		var csrf string
		if err == nil {
			d.smu.Lock()
			csrf = d.sessions[c.Value]
			d.smu.Unlock()
		}
		if csrf == "" {
			http.Error(w, "open the link cinema dashboard printed", http.StatusUnauthorized)
			return
		}
		if req.Method != http.MethodGet &&
			subtle.ConstantTimeCompare([]byte(req.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
			http.Error(w, "bad csrf token", http.StatusForbidden)
			return
		}
		next(w, req, csrf)
	}
}

type frame struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Covers bool   `json:"covers"` // this run is what makes the task reusable for the current tree
}

type dashTask struct {
	taskStatus
	Argv    []string `json:"argv"`
	Frames  []frame  `json:"frames"`
	Running bool     `json:"running"`
}

func (d *dashSrv) state(w http.ResponseWriter, _ *http.Request, csrf string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.r
	r.fresh()
	r.cfg, r.cfgErr = task.Load(r.root)
	head, dirty := headState(r.root)
	out := map[string]any{"repo": r.root, "head": head, "dirty": dirty, "platform": task.Platform(), "csrf": csrf}
	switch {
	case errors.Is(r.cfgErr, task.ErrNoConfig):
		out["config_error"] = map[string]string{"reason_code": plan.PolicyUnavailable, "message": "No .cinema/tasks.yaml yet. Run `cinema init` in the repo."}
	case r.cfgErr != nil:
		out["config_error"] = map[string]string{"reason_code": plan.PolicyInvalid, "message": r.cfgErr.Error()}
	default:
		d.smu.Lock()
		running := maps(d.running)
		d.smu.Unlock()
		var tasks []dashTask
		for _, id := range sortedIDs(r.cfg) {
			t := r.cfg.Tasks[id]
			ts, _, runs := decide(r, t)
			dt := dashTask{taskStatus: ts, Argv: t.Argv, Running: running[id]}
			for i, run := range runs {
				if i == 12 {
					break
				}
				dt.Frames = append(dt.Frames, frame{run.ID, run.State, ts.State == plan.Reusable && run.ID == ts.RunID})
			}
			tasks = append(tasks, dt)
		}
		out["tasks"] = tasks
	}
	runs, err := r.st.RecentRuns(r.id, 40)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out["runs"] = runs
	writeJSONResponse(w, out)
}

const maxLogBytes = 512 << 10

func (d *dashSrv) runDetail(w http.ResponseWriter, req *http.Request, _ string) {
	d.mu.Lock()
	run, stdout, stderr, err := d.r.st.Run(d.r.id, req.PathValue("id"))
	d.mu.Unlock()
	if err != nil {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}
	so, soCut := cleanLog(stdout)
	se, seCut := cleanLog(stderr)
	writeJSONResponse(w, map[string]any{"run": run, "stdout": so, "stderr": se,
		"stdout_clipped": soCut, "stderr_clipped": seCut})
}

// events tells the page when to re-fetch: a run started/finished/was cleaned
// up, the working tree changed, or tasks.yaml changed. One poll loop per page.
// ponytail: per-connection polling; share one poller if many tabs ever matter.
func (d *dashSrv) events(w http.ResponseWriter, req *http.Request, _ string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	last := ""
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	beat := time.Now()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-d.ctx.Done():
			return
		case <-tick.C:
		}
		sig := d.signature()
		switch {
		case sig != last && last != "":
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
		case time.Since(beat) > 15*time.Second:
			fmt.Fprint(w, ": keepalive\n\n")
		default:
			last = sig
			continue
		}
		last, beat = sig, time.Now()
		fl.Flush()
	}
}

func (d *dashSrv) signature() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.r.fresh()
	tree, _ := d.r.source(nil)
	cfg, _ := os.ReadFile(d.r.root + "/" + task.ConfigPath)
	d.smu.Lock()
	n := len(d.running)
	d.smu.Unlock()
	return fmt.Sprintf("%s|%s|%x|%d", d.r.st.Signature(d.r.id), tree, cfg, n)
}

// startRun runs `cinema run <task>` in the working tree, exactly as the CLI would.
func (d *dashSrv) startRun(w http.ResponseWriter, req *http.Request, _ string) {
	var body struct{ Task string }
	if json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024)).Decode(&body) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	ok := d.r.cfg != nil && d.r.cfg.Tasks[body.Task] != nil
	d.mu.Unlock()
	if !ok {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	d.smu.Lock()
	if d.running[body.Task] {
		d.smu.Unlock()
		http.Error(w, "already running", http.StatusConflict)
		return
	}
	d.running[body.Task] = true
	d.smu.Unlock()

	self, err := os.Executable()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cmd := exec.CommandContext(d.ctx, self, "run", "--", body.Task)
	cmd.Dir = d.r.root
	// On dashboard shutdown, let `cinema run` stop its task and clean up.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		d.smu.Lock()
		delete(d.running, body.Task)
		d.smu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		_ = cmd.Wait() // output and result are in the run record
		d.smu.Lock()
		delete(d.running, body.Task)
		d.smu.Unlock()
	}()
	w.WriteHeader(http.StatusAccepted)
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// cleanLog makes captured output safe and readable as plain text: ANSI escapes
// and control characters removed, carriage-return redraws collapsed, and only
// the last maxLogBytes kept (failures print last). The page renders it as text.
func cleanLog(b []byte) (string, bool) {
	clipped := len(b) > maxLogBytes
	if clipped {
		b = b[len(b)-maxLogBytes:]
	}
	s := ansiRE.ReplaceAllString(strings.ToValidUTF8(string(b), "�"), "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			l = l[j+1:]
		}
		lines[i] = strings.Map(func(r rune) rune {
			if r < 0x20 && r != '\t' || r == 0x7f {
				return -1
			}
			return r
		}, l)
	}
	return strings.Join(lines, "\n"), clipped
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func maps(m map[string]bool) map[string]bool {
	c := make(map[string]bool, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	} else if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return // headless (e.g. a VPS): just print the link
	}
	_ = exec.Command(name, url).Start()
}
