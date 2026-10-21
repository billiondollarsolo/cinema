package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestDash(t *testing.T) (*dashSrv, http.Handler) {
	e := newEnv(t)
	t.Setenv("CINEMA_DATA_DIR", e.data)
	r, err := openRepoAt(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	d := newDash(context.Background(), r, 7777, []string{"vps.tailnet.ts.net"})
	return d, d.handler()
}

func do(h http.Handler, method, path, host, origin, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func login(t *testing.T, d *dashSrv, h http.Handler) *http.Cookie {
	w := do(h, "POST", "/api/session", "127.0.0.1:7777", "http://127.0.0.1:7777", `{"token":"`+d.token+`"}`, nil, "")
	if w.Code != 204 {
		t.Fatalf("login: %d", w.Code)
	}
	c := w.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: %+v", c)
	}
	return c
}

func TestDashboardGuards(t *testing.T) { // ACC-047
	d, h := newTestDash(t)
	const host, origin = "127.0.0.1:7777", "http://127.0.0.1:7777"

	if w := do(h, "GET", "/", "evil.example:7777", "", "", nil, ""); w.Code != 403 {
		t.Errorf("rebinding Host: %d", w.Code)
	}
	for _, okHost := range []string{host, "localhost:7777", "vps.tailnet.ts.net"} {
		if w := do(h, "GET", "/", okHost, "", "", nil, ""); w.Code != 200 {
			t.Errorf("allowed host %s: %d", okHost, w.Code)
		}
	}
	if w := do(h, "GET", "/", host, "", "", nil, ""); !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Error("missing CSP")
	}
	if w := do(h, "POST", "/api/session", host, "http://evil.example", `{"token":"`+d.token+`"}`, nil, ""); w.Code != 403 {
		t.Errorf("foreign Origin on session: %d", w.Code)
	}
	if w := do(h, "POST", "/api/session", host, "", `{"token":"`+d.token+`"}`, nil, ""); w.Code != 403 {
		t.Errorf("missing Origin on POST: %d", w.Code)
	}
	if w := do(h, "POST", "/api/session", host, origin, `{"token":"nope"}`, nil, ""); w.Code != 401 {
		t.Errorf("bad token: %d", w.Code)
	}
	if w := do(h, "GET", "/api/state", host, "", "", nil, ""); w.Code != 401 {
		t.Errorf("no cookie: %d", w.Code)
	}

	c := login(t, d, h)
	if w := do(h, "GET", "/api/state", host, "", "", c, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"csrf"`) {
		t.Errorf("state with cookie: %d", w.Code)
	}
	if w := do(h, "POST", "/api/run", host, origin, `{"task":"check"}`, c, ""); w.Code != 403 {
		t.Errorf("run without CSRF token: %d", w.Code)
	}
	if w := do(h, "POST", "/api/run", host, origin, `{"task":"check"}`, c, "wrong"); w.Code != 403 {
		t.Errorf("run with wrong CSRF token: %d", w.Code)
	}
	if w := do(h, "POST", "/api/run", host, "http://evil.example", `{"task":"check"}`, c, d.sessions[c.Value]); w.Code != 403 {
		t.Errorf("run from foreign Origin: %d", w.Code)
	}
	if w := do(h, "GET", "/api/runs/../../etc/passwd", host, "", "", c, ""); w.Code == 200 {
		t.Error("path traversal in run id")
	}
}

func TestCleanLog(t *testing.T) { // ACC-048
	in := "\x1b[31mred\x1b[0m <script>x</script>\x1b]0;title\x07\n50%\r100%\nbell\x07 nul\x00\n"
	got, clipped := cleanLog([]byte(in))
	want := "red <script>x</script>\n100%\nbell nul\n"
	if got != want || clipped {
		t.Fatalf("got %q", got)
	}
	big := strings.Repeat("x", maxLogBytes+10)
	if got, clipped := cleanLog([]byte(big)); len(got) != maxLogBytes || !clipped {
		t.Fatalf("clip: len %d clipped %v", len(got), clipped)
	}
}
