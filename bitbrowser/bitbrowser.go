// Package bitbrowser exposes BitBrowser's local API on a probe agent's machine.
// Transport is the agent TCP tunnel; this plugin adds the OpenAPI document, an
// operation-level call API with validation / confirm / audit, a transparent
// passthrough for existing clients, and a debug page.
package bitbrowser

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Necho-dev/vartio-plugin-bitbrowser/sdk"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultTarget = "127.0.0.1:54345"
	maxBody       = 8 << 20
	maxResponse   = 64 << 20
)

//go:embed openapi.json
var spec []byte

//go:embed ui
var uiFS embed.FS

type Operation struct {
	ID       string         `json:"operationId"`
	Path     string         `json:"path"`
	Tag      string         `json:"tag,omitempty"`
	Summary  string         `json:"summary"`
	Required []string       `json:"required,omitempty"`
	Confirm  bool           `json:"confirm,omitempty"`
	Audit    bool           `json:"audit,omitempty"`
	NoAuth   bool           `json:"noAuth,omitempty"`
	Example  map[string]any `json:"example"`
}

type Plugin struct {
	host   sdk.Host
	target string
	apiKey string
	ops    map[string]*Operation
	byPath map[string]*Operation
	order  []*Operation
}

// New reads BITBROWSER_ADDR (agent-side host:port, default 127.0.0.1:54345) and
// BITBROWSER_API_KEY (used when a request carries no x-api-key).
func New() *Plugin {
	target := strings.TrimSpace(os.Getenv("BITBROWSER_ADDR"))
	if target == "" {
		target = DefaultTarget
	}
	return &Plugin{target: target, apiKey: strings.TrimSpace(os.Getenv("BITBROWSER_API_KEY"))}
}

func (p *Plugin) Manifest() sdk.Manifest {
	return sdk.Manifest{
		Name:        "bitbrowser",
		Title:       "比特浏览器",
		Version:     "1.0.0",
		Description: "经探针 TCP 隧道调用远端机器上的比特浏览器本地 API（" + p.target + "）",
		Primitives:  []string{sdk.CapTunnelTCP},
		Targets:     []string{p.target},
	}
}

func (p *Plugin) Init(_ context.Context, host sdk.Host) error {
	p.host = host
	ops, err := parseOperations(spec)
	if err != nil {
		return err
	}
	p.ops, p.byPath, p.order = map[string]*Operation{}, map[string]*Operation{}, ops
	for _, op := range ops {
		p.ops[op.ID] = op
		p.byPath[op.Path] = op
	}
	return nil
}

func (p *Plugin) UI() fs.FS {
	sub, _ := fs.Sub(uiFS, "ui")
	return sub
}

func (p *Plugin) Routes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(spec)
	})
	mux.HandleFunc("GET "+prefix+"/operations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"target": p.target, "apiKeyDefault": p.apiKey != "", "operations": p.order})
	})
	mux.HandleFunc("GET "+prefix+"/agents/{id}/status", p.handleStatus)
	mux.HandleFunc("POST "+prefix+"/agents/{id}/call/{op}", p.handleCall)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		mux.HandleFunc(m+" "+prefix+"/agents/{id}/api/{path...}", p.handlePassthrough)
	}
}

// Status reports whether BitBrowser answers on the agent's machine.
type Status struct {
	Agent     string  `json:"agent"`
	Target    string  `json:"target"`
	Reachable bool    `json:"reachable"`
	LatencyMs float64 `json:"latencyMs,omitempty"`
	Message   string  `json:"message,omitempty"`
}

func (p *Plugin) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := p.host.Agent(id); !ok {
		sdk.WriteError(w, sdk.ErrOffline())
		return
	}
	st := Status{Agent: id, Target: p.target}
	start := time.Now()
	status, body, err := p.send(r.Context(), id, http.MethodPost, "/health", "", []byte("{}"), "")
	switch {
	case err != nil:
		if api := asAPI(err); api != nil && (api.Code == "cap_missing" || api.Code == "guest_offline" || api.Code == "guest_unbound") {
			sdk.WriteError(w, api)
			return
		}
		st.Message = errMessage(err)
	case status != http.StatusOK:
		st.Message = fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 200))
	default:
		var res struct {
			Success bool   `json:"success"`
			Data    any    `json:"data"`
			Msg     string `json:"msg"`
		}
		_ = json.Unmarshal(body, &res)
		st.Reachable = res.Success
		st.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
		if s, ok := res.Data.(string); ok {
			st.Message = s
		} else {
			st.Message = res.Msg
		}
	}
	writeJSON(w, st)
}

func (p *Plugin) handleCall(w http.ResponseWriter, r *http.Request) {
	op := p.ops[r.PathValue("op")]
	if op == nil {
		sdk.WriteError(w, &sdk.Error{Status: http.StatusNotFound, Code: "not_found", Message: "unknown operation " + r.PathValue("op")})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		sdk.WriteError(w, sdk.ErrBad("read body: %v", err))
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		sdk.WriteError(w, sdk.ErrBad("body must be a JSON object"))
		return
	}
	var missing []string
	for _, k := range op.Required {
		if _, ok := params[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sdk.WriteError(w, sdk.ErrBad("%s requires %s", op.ID, strings.Join(missing, ", ")))
		return
	}
	p.forward(w, r, op, http.MethodPost, op.Path, "", body)
}

func (p *Plugin) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		sdk.WriteError(w, sdk.ErrBad("read body: %v", err))
		return
	}
	path := "/" + r.PathValue("path")
	p.forward(w, r, p.byPath[path], r.Method, path, r.URL.RawQuery, body)
}

// forward sends one request to BitBrowser and copies its response back verbatim.
func (p *Plugin) forward(w http.ResponseWriter, r *http.Request, op *Operation, method, path, query string, body []byte) {
	id := r.PathValue("id")
	if op != nil && op.Confirm && !confirmed(r) {
		sdk.WriteError(w, sdk.RequireConfirm(false))
		return
	}
	key := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	if key == "" {
		key = p.apiKey
	}
	status, resp, err := p.send(r.Context(), id, method, path, query, body, key)
	if op != nil && op.Audit {
		detail := path
		if s := idsOf(body); s != "" {
			detail += " " + s
		}
		auditErr := err
		if auditErr == nil {
			auditErr = resultError(status, resp)
		}
		p.host.Audit(r, id, op.ID, detail, auditErr)
	}
	if err != nil {
		sdk.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(resp)
}

func (p *Plugin) send(ctx context.Context, id, method, path, query string, body []byte, key string) (int, []byte, error) {
	u := "http://" + p.target + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, sdk.ErrBad("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := p.host.HTTPClient(id, p.target).Do(req)
	if err != nil {
		if api := asAPI(err); api != nil {
			return 0, nil, api
		}
		return 0, nil, &sdk.Error{Status: http.StatusBadGateway, Code: "upstream", Message: err.Error()}
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, &sdk.Error{Status: http.StatusBadGateway, Code: "upstream", Message: err.Error()}
	}
	return resp.StatusCode, out, nil
}

// resultError turns BitBrowser's {success:false,msg} into an audit error.
func resultError(status int, body []byte) error {
	var res struct {
		Success *bool  `json:"success"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal(body, &res)
	if status >= 400 {
		return fmt.Errorf("HTTP %d %s", status, truncate(res.Msg, 200))
	}
	if res.Success != nil && !*res.Success {
		return fmt.Errorf("%s", truncate(res.Msg, 200))
	}
	return nil
}

func idsOf(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var parts []string
	for _, k := range []string{"id", "browserId", "ids", "browserIds", "groupName"} {
		if v, ok := m[k]; ok {
			b, _ := json.Marshal(v)
			parts = append(parts, k+"="+truncate(string(b), 120))
		}
	}
	return strings.Join(parts, " ")
}

func confirmed(r *http.Request) bool {
	for _, v := range []string{r.Header.Get("X-Confirm"), r.URL.Query().Get("confirm")} {
		if b, _ := strconv.ParseBool(strings.TrimSpace(v)); b {
			return true
		}
	}
	return false
}

func asAPI(err error) *sdk.Error {
	var api *sdk.Error
	if errors.As(err, &api) {
		return api
	}
	return nil
}

func errMessage(err error) string {
	if api := asAPI(err); api != nil {
		return api.Code + ": " + api.Message
	}
	return err.Error()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// parseOperations flattens the OpenAPI paths into operations, resolving local
// $refs far enough to know required fields and to build an example body.
func parseOperations(raw []byte) ([]*Operation, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("openapi.json: %w", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	var out []*Operation
	for path, item := range paths {
		post, _ := item.(map[string]any)["post"].(map[string]any)
		if post == nil {
			continue
		}
		op := &Operation{Path: path}
		op.ID, _ = post["operationId"].(string)
		op.Summary, _ = post["summary"].(string)
		if tags, _ := post["tags"].([]any); len(tags) > 0 {
			op.Tag, _ = tags[0].(string)
		}
		op.Confirm, _ = post["x-confirm"].(bool)
		op.Audit, _ = post["x-audit"].(bool)
		if sec, ok := post["security"].([]any); ok && len(sec) == 0 {
			op.NoAuth = true
		}
		if op.ID == "" {
			return nil, fmt.Errorf("openapi.json: %s has no operationId", path)
		}
		schema := map[string]any{}
		if rb := resolve(doc, post["requestBody"]); rb != nil {
			if content, _ := rb["content"].(map[string]any); content != nil {
				if mt, _ := content["application/json"].(map[string]any); mt != nil {
					if s := resolve(doc, mt["schema"]); s != nil {
						schema = s
					}
				}
			}
		}
		op.Required = required(doc, schema)
		op.Example = example(doc, schema, op.Required)
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tag != out[j].Tag {
			return out[i].Tag < out[j].Tag
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func resolve(doc map[string]any, node any) map[string]any {
	m, _ := node.(map[string]any)
	for i := 0; m != nil && i < 8; i++ {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		var cur any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cm, _ := cur.(map[string]any)
			cur = cm[part]
		}
		m, _ = cur.(map[string]any)
	}
	return m
}

func required(doc, schema map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s map[string]any) {
		list, _ := s["required"].([]any)
		for _, v := range list {
			if k, ok := v.(string); ok && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	add(schema)
	all, _ := schema["allOf"].([]any)
	for _, a := range all {
		if s := resolve(doc, a); s != nil {
			add(s)
		}
	}
	return out
}

// example fills required fields with defaults or type-shaped placeholders.
func example(doc, schema map[string]any, req []string) map[string]any {
	props := map[string]any{}
	collect := func(s map[string]any) {
		p, _ := s["properties"].(map[string]any)
		for k, v := range p {
			props[k] = v
		}
	}
	all, _ := schema["allOf"].([]any)
	for _, a := range all {
		if s := resolve(doc, a); s != nil {
			collect(s)
		}
	}
	collect(schema)
	out := map[string]any{}
	for _, k := range req {
		ps := resolve(doc, props[k])
		if ps == nil {
			out[k] = ""
			continue
		}
		if d, ok := ps["default"]; ok {
			out[k] = d
			continue
		}
		t := ps["type"]
		if list, ok := t.([]any); ok && len(list) > 0 {
			t = list[0]
		}
		switch t {
		case "integer", "number":
			out[k] = 0
		case "boolean":
			out[k] = false
		case "array":
			out[k] = []any{}
		case "object":
			out[k] = map[string]any{}
		default:
			out[k] = ""
		}
	}
	return out
}
