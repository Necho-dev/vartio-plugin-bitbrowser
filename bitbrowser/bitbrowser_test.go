package bitbrowser

import (
	"context"
	"encoding/json"
	"github.com/Necho-dev/vartio/sdk"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHost dials the fake BitBrowser directly instead of through an agent.
type fakeHost struct {
	addr   string
	mu     sync.Mutex
	audits []string
}

func (h *fakeHost) Agents() []sdk.SessionInfo { return []sdk.SessionInfo{{DeviceID: "dev1"}} }
func (h *fakeHost) Agent(id string) (sdk.SessionInfo, bool) {
	return sdk.SessionInfo{DeviceID: id, Online: true}, id == "dev1"
}
func (h *fakeHost) DialTCP(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", h.addr)
}
func (h *fakeHost) HTTPClient(id, target string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return h.DialTCP(ctx, id, target)
	}}}
}
func (h *fakeHost) Call(context.Context, string, string, any) (json.RawMessage, error) {
	return nil, io.ErrUnexpectedEOF
}
func (h *fakeHost) Audit(_ *http.Request, id, kind, detail string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := id + " " + kind + " " + detail
	if err != nil {
		s += " ERR " + err.Error()
	}
	h.audits = append(h.audits, s)
}

type seen struct {
	path, key, body string
}

func setup(t *testing.T) (*httptest.Server, *fakeHost, chan seen) {
	t.Helper()
	got := make(chan seen, 16)
	bb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.URL.Path, r.Header.Get("x-api-key"), string(b)}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, `{"success":true,"data":"the server is running good."}`)
		case "/browser/delete":
			_, _ = io.WriteString(w, `{"success":false,"msg":"not found"}`)
		case "/nope":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"statusCode":404}`)
		default:
			_, _ = io.WriteString(w, `{"success":true,"data":{"list":[]}}`)
		}
	}))
	t.Cleanup(bb.Close)
	host := &fakeHost{addr: strings.TrimPrefix(bb.URL, "http://")}
	t.Setenv("BITBROWSER_API_KEY", "hub-default")
	p := New()
	if err := p.Init(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// probe-hub serves its monitor page on "GET /"; plugin routes must not conflict with it.
	mux.HandleFunc("GET /", http.NotFound)
	p.Routes(mux, sdk.PathPrefix+"/bitbrowser")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, host, got
}

func do(t *testing.T, method, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSpecOperations(t *testing.T) {
	ops, err := parseOperations(spec)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Operation{}
	for _, op := range ops {
		if byID[op.ID] != nil {
			t.Fatalf("duplicate operationId %s", op.ID)
		}
		byID[op.ID] = op
	}
	if len(ops) < 30 {
		t.Fatalf("only %d operations", len(ops))
	}
	list := byID["browserList"]
	if list == nil || strings.Join(list.Required, ",") != "page,pageSize" || list.Example["pageSize"] != float64(10) {
		t.Fatalf("browserList %+v", list)
	}
	if op := byID["browserOpen"]; op == nil || op.Required[0] != "id" || op.Example["id"] != "" || !op.Audit {
		t.Fatalf("browserOpen %+v", op)
	}
	if op := byID["browserUpdatePartial"]; op == nil || strings.Join(op.Required, ",") != "ids" {
		t.Fatalf("allOf required %+v", op)
	}
	for _, id := range []string{"browserDelete", "browserDeleteIds", "browserCloseAll", "groupDelete", "cacheClear"} {
		if !byID[id].Confirm {
			t.Errorf("%s should need confirm", id)
		}
	}
	if !byID["health"].NoAuth || byID["browserList"].NoAuth {
		t.Fatal("security flags")
	}
}

func TestCallValidatesForwardsAndAudits(t *testing.T) {
	srv, host, got := setup(t)
	base := srv.URL + sdk.PathPrefix + "/bitbrowser/agents/dev1"

	code, body := do(t, "POST", base+"/call/browserList", `{"page":0}`, nil)
	if code != 400 || !strings.Contains(body, "pageSize") {
		t.Fatalf("missing field: %d %s", code, body)
	}
	code, body = do(t, "POST", base+"/call/browserList", `[1]`, nil)
	if code != 400 {
		t.Fatalf("non-object body: %d %s", code, body)
	}
	code, _ = do(t, "POST", base+"/call/nope", `{}`, nil)
	if code != 404 {
		t.Fatalf("unknown op: %d", code)
	}

	code, body = do(t, "POST", base+"/call/browserList", `{"page":0,"pageSize":5}`, map[string]string{"X-Api-Key": "caller-key"})
	if code != 200 || !strings.Contains(body, `"list"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	if s := <-got; s.path != "/browser/list" || s.key != "caller-key" || s.body != `{"page":0,"pageSize":5}` {
		t.Fatalf("upstream saw %+v", s)
	}

	code, _ = do(t, "POST", base+"/call/browserList", `{"page":0,"pageSize":5}`, nil)
	if s := <-got; code != 200 || s.key != "hub-default" {
		t.Fatalf("default key: %d %+v", code, s)
	}

	code, body = do(t, "POST", base+"/call/browserDelete", `{"id":"x"}`, nil)
	if code != 403 || !strings.Contains(body, "confirm") {
		t.Fatalf("delete without confirm: %d %s", code, body)
	}
	select {
	case s := <-got:
		t.Fatalf("delete reached BitBrowser without confirm: %+v", s)
	default:
	}
	code, _ = do(t, "POST", base+"/call/browserDelete", `{"id":"x"}`, map[string]string{"X-Confirm": "true"})
	<-got
	if code != 200 {
		t.Fatalf("confirmed delete: %d", code)
	}
	host.mu.Lock()
	audits := strings.Join(host.audits, "\n")
	host.mu.Unlock()
	if !strings.Contains(audits, `dev1 browserDelete /browser/delete id="x" ERR not found`) {
		t.Fatalf("audit %q", audits)
	}
	if strings.Contains(audits, "browserList") {
		t.Fatalf("read-only call audited: %q", audits)
	}
}

func TestPassthroughAndStatus(t *testing.T) {
	srv, _, got := setup(t)
	base := srv.URL + sdk.PathPrefix + "/bitbrowser/agents/dev1"

	code, body := do(t, "POST", base+"/api/browser/ports", `{}`, map[string]string{"x-api-key": "k"})
	if s := <-got; code != 200 || s.path != "/browser/ports" || s.key != "k" || !strings.Contains(body, "success") {
		t.Fatalf("passthrough %d %s %+v", code, body, s)
	}
	code, _ = do(t, "POST", base+"/api/nope", `{}`, nil)
	<-got
	if code != 404 {
		t.Fatalf("upstream status not kept: %d", code)
	}
	code, _ = do(t, "POST", base+"/api/group/delete", `{"id":"g"}`, nil)
	if code != 403 {
		t.Fatalf("passthrough must honour x-confirm: %d", code)
	}

	code, body = do(t, "GET", base+"/status", "", nil)
	<-got
	var st Status
	_ = json.Unmarshal([]byte(body), &st)
	if code != 200 || !st.Reachable || st.Message != "the server is running good." {
		t.Fatalf("status %d %s", code, body)
	}
	code, _ = do(t, "GET", srv.URL+sdk.PathPrefix+"/bitbrowser/agents/ghost/status", "", nil)
	if code != 409 {
		t.Fatalf("offline agent status: %d", code)
	}

	code, body = do(t, "GET", srv.URL+sdk.PathPrefix+"/bitbrowser/openapi.json", "", nil)
	var doc map[string]any
	if code != 200 || json.Unmarshal([]byte(body), &doc) != nil || doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi %d", code)
	}
}
