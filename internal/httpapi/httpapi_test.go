package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"task045-lognorm/internal/lognorm"
)

func newTestAPI(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	svc := lognorm.NewService()
	srv := httptest.NewServer(New(svc).Handler())
	return srv, srv.Client()
}

func do(t *testing.T, c *http.Client, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		r = bytes.NewReader(buf)
	} else {
		r = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHealthz(t *testing.T) {
	srv, c := newTestAPI(t)
	defer srv.Close()
	resp, err := c.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
}

func TestIngestAndStats(t *testing.T) {
	srv, c := newTestAPI(t)
	defer srv.Close()
	code, out := do(t, c, http.MethodPost, srv.URL+"/ingest",
		map[string]any{"lines": []string{
			`{"level":"info","msg":"a"}`,
			`level=warn msg=b`,
			`[ERROR] boom`,
			`plain`,
		}})
	if code != http.StatusOK {
		t.Fatalf("ingest: code=%d body=%v", code, out)
	}
	if n, _ := out["ingested"].(float64); n != 4 {
		t.Errorf("ingested=%v want 4", out["ingested"])
	}
	code, out = do(t, c, http.MethodGet, srv.URL+"/stats", nil)
	if code != http.StatusOK {
		t.Fatalf("stats: code=%d", code)
	}
	byLevel, _ := out["by_level"].(map[string]any)
	if byLevel["INFO"].(float64) != 2 || byLevel["WARN"].(float64) != 1 || byLevel["ERROR"].(float64) != 1 {
		t.Errorf("by_level=%v", byLevel)
	}
}

func TestIngest_BadBody(t *testing.T) {
	srv, c := newTestAPI(t)
	defer srv.Close()
	// malformed JSON
	if code, _ := do(t, c, http.MethodPost, srv.URL+"/ingest", map[string]any{}); code != http.StatusBadRequest {
		t.Errorf("missing lines: code=%d want 400", code)
	}
	// unknown field
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ingest", bytes.NewReader([]byte(`{"foo":1}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field: code=%d want 400", resp.StatusCode)
	}
}

func TestLogs_MinLevelInvalid(t *testing.T) {
	srv, c := newTestAPI(t)
	defer srv.Close()
	if code, _ := do(t, c, http.MethodGet, srv.URL+"/logs?min_level=FOO", nil); code != http.StatusBadRequest {
		t.Errorf("min_level=FOO: code=%d want 400", code)
	}
}

func TestLogs_TimeInvalid(t *testing.T) {
	srv, c := newTestAPI(t)
	defer srv.Close()
	if code, _ := do(t, c, http.MethodGet, srv.URL+"/logs?since=bad", nil); code != http.StatusBadRequest {
		t.Errorf("since=bad: code=%d want 400", code)
	}
	if code, _ := do(t, c, http.MethodGet, srv.URL+"/logs?until=bad", nil); code != http.StatusBadRequest {
		t.Errorf("until=bad: code=%d want 400", code)
	}
}

func TestLogs_LimitInvalid(t *testing.T) {
	srv, c := newTestAPI(t)
	defer srv.Close()
	if code, _ := do(t, c, http.MethodGet, srv.URL+"/logs?limit=0", nil); code != http.StatusBadRequest {
		t.Errorf("limit=0: code=%d want 400", code)
	}
	if code, _ := do(t, c, http.MethodGet, srv.URL+"/logs?limit=abc", nil); code != http.StatusBadRequest {
		t.Errorf("limit=abc: code=%d want 400", code)
	}
	if code, _ := do(t, c, http.MethodGet, srv.URL+"/logs?limit=-5", nil); code != http.StatusBadRequest {
		t.Errorf("limit=-5: code=%d want 400", code)
	}
}

func TestGetByID_404(t *testing.T) {
	srv, c := newTestAPI(t)
	defer srv.Close()
	if code, _ := do(t, c, http.MethodGet, srv.URL+"/logs/log-999999", nil); code != http.StatusNotFound {
		t.Errorf("get missing: code=%d want 404", code)
	}
}
