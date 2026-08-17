// Package selfcheck runs an end-to-end verification of the lognorm service
// against an in-process HTTP server. It is invoked by the --smoke-test flag
// and exits the process on completion.
//
// Each scenario builds its own fresh service+server so global state (the
// stored log buffer and the id counter) never leaks between scenarios — this
// keeps id assignment deterministic (always starts at log-000001) and avoids
// cross-scenario contamination of /logs and /stats results.
package selfcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"task045-lognorm/internal/httpapi"
	"task045-lognorm/internal/lognorm"
)

// client wraps a fresh httptest server bound to a fresh service.
type client struct {
	base string
	c    *http.Client
	srv  *httptest.Server
}

func newClient() *client {
	svc := lognorm.NewService()
	srv := httptest.NewServer(httpapi.New(svc).Handler())
	return &client{base: srv.URL, c: srv.Client(), srv: srv}
}

func (cl *client) close() { cl.srv.Close() }

func (cl *client) post(path string, body any) (int, map[string]any) {
	buf, _ := json.Marshal(body)
	return cl.postRaw(path, buf)
}

func (cl *client) postRaw(path string, raw []byte) (int, map[string]any) {
	req, _ := http.NewRequest(http.MethodPost, cl.base+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.c.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	return readBody(resp)
}

func (cl *client) get(path string) (int, map[string]any) {
	resp, err := cl.c.Get(cl.base + path)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	return readBody(resp)
}

func readBody(resp *http.Response) (int, map[string]any) {
	data, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return resp.StatusCode, out
}

// eqFloat compares a JSON-decoded number to an expected float.
func eqFloat(v any, want float64) bool {
	f, ok := v.(float64)
	return ok && f == want
}

// logsList extracts the "logs" array from a /logs response.
func logsList(body map[string]any) []map[string]any {
	arr, _ := body["logs"].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Run exercises the full HTTP API across isolated scenarios, returning nil if
// every behavior matches the specification.
func Run() error {
	scenarios := []struct {
		name string
		fn   func() error
	}{
		{"健康检查", scenarioHealth},
		{"JSON 归一化与字段提取", scenarioJSON},
		{"JSON 别名键提取", scenarioJSONAliases},
		{"logfmt 解析含引号与裸 token", scenarioLogfmt},
		{"bracket 带 ts 与不带 ts", scenarioBracket},
		{"plain 回退与 ts_source=ingest", scenarioPlain},
		{"形似 JSON 解析失败降级 plain", scenarioJSONFallback},
		{"级别别名归一化", scenarioLevelAliases},
		{"无法识别级别拒绝入库", scenarioBadLevelRejected},
		{"单行超长拒绝", scenarioLineTooLong},
		{"空行静默跳过", scenarioBlankSkipped},
		{"min_level 阈值过滤", scenarioMinLevelFilter},
		{"since/until 半开区间", scenarioHalfOpenTime},
		{"q 子串过滤", scenarioQFilter},
		{"format 过滤", scenarioFormatFilter},
		{"时间排序与 id 平局", scenarioSortOrder},
		{"stats 聚合", scenarioStats},
		{"按 id 查询与 404", scenarioGetByID},
		{"min_level 非法 400", scenarioMinLevelInvalid},
		{"since/until 非法 400", scenarioTimeInvalid},
		{"limit 截断", scenarioLimit},
		{"limit 非法 400", scenarioLimitInvalid},
	}
	for _, sc := range scenarios {
		if err := runScenario(sc.fn); err != nil {
			return fmt.Errorf("%s: %w", sc.name, err)
		}
	}
	return nil
}

// runScenario invokes a scenario function, converting any panic into an error
// so a faulty assertion surfaces as a clean failure rather than a crash.
func runScenario(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

func scenarioHealth() error {
	cl := newClient()
	defer cl.close()
	code, body := cl.get("/healthz")
	if code != http.StatusOK || body["status"] != "ok" {
		return fmt.Errorf("healthz: code=%d body=%v", code, body)
	}
	return nil
}

func scenarioJSON() error {
	cl := newClient()
	defer cl.close()
	code, body := cl.post("/ingest", map[string]any{"lines": []string{
		`{"ts":"2026-08-16T10:00:00Z","level":"info","msg":"request completed","app":"api","status":200}`,
	}})
	if code != http.StatusOK || !eqFloat(body["ingested"], 1) {
		return fmt.Errorf("ingest: code=%d body=%v", code, body)
	}
	code, body = cl.get("/logs/log-000001")
	if code != http.StatusOK {
		return fmt.Errorf("get: code=%d body=%v", code, body)
	}
	if body["format"] != "json" || body["level"] != "INFO" || body["level_raw"] != "info" || body["msg"] != "request completed" {
		return fmt.Errorf("json record: %v", body)
	}
	if body["ts"] != "2026-08-16T10:00:00Z" || body["ts_source"] != "parsed" {
		return fmt.Errorf("json ts: %v %v", body["ts"], body["ts_source"])
	}
	fields, _ := body["fields"].(map[string]any)
	if fields["app"] != "api" || !eqFloat(fields["status"], 200) {
		return fmt.Errorf("json fields: %v", fields)
	}
	return nil
}

func scenarioJSONAliases() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		`{"time":"2026-08-16T10:00:00Z","lvl":"warn","message":"hi","app":"api","severity":"error"}`,
	}})
	if !eqFloat(body["ingested"], 1) {
		return fmt.Errorf("ingest: %v", body)
	}
	_, body = cl.get("/logs/log-000001")
	// lvl (warn) wins over severity (error) per alias-list order.
	if body["level"] != "WARN" || body["level_raw"] != "warn" {
		return fmt.Errorf("level=%v raw=%v want WARN/warn", body["level"], body["level_raw"])
	}
	if body["msg"] != "hi" {
		return fmt.Errorf("msg=%v", body["msg"])
	}
	if body["ts"] != "2026-08-16T10:00:00Z" || body["ts_source"] != "parsed" {
		return fmt.Errorf("ts=%v src=%v", body["ts"], body["ts_source"])
	}
	fields, _ := body["fields"].(map[string]any)
	if fields["app"] != "api" {
		return fmt.Errorf("fields=%v", fields)
	}
	return nil
}

func scenarioLogfmt() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		`level=info msg="hello world" status=200 app=api`,
		`level=warn msg=hi started`,
	}})
	if !eqFloat(body["ingested"], 2) {
		return fmt.Errorf("ingest: %v", body)
	}
	logs := logsList(mustGet(cl, "/logs"))
	if len(logs) != 2 {
		return fmt.Errorf("len=%d want 2", len(logs))
	}
	r0 := logs[0]
	if r0["format"] != "logfmt" || r0["level"] != "INFO" || r0["msg"] != "hello world" {
		return fmt.Errorf("log0: %v", r0)
	}
	f0, _ := r0["fields"].(map[string]any)
	if f0["status"] != "200" || f0["app"] != "api" {
		return fmt.Errorf("log0 fields: %v", f0)
	}
	// Bare token "started" decodes to a JSON boolean true.
	r1 := logs[1]
	f1, _ := r1["fields"].(map[string]any)
	if f1["started"] != true {
		return fmt.Errorf("bare token started=%v want true", f1["started"])
	}
	return nil
}

func scenarioBracket() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		`[ERROR] 2026-08-16T12:00:00Z disk full`,
		`[WARN] slow query`,
	}})
	if !eqFloat(body["ingested"], 2) {
		return fmt.Errorf("ingest: %v", body)
	}
	logs := logsList(mustGet(cl, "/logs"))
	if len(logs) != 2 {
		return fmt.Errorf("len=%d want 2", len(logs))
	}
	// Sorted by ts: the WARN record has ingest ts (2026-08-16T09:00:00Z-ish),
	// the ERROR record has 12:00. Ingest ts is non-deterministic, so sort by
	// level content instead.
	var errRec, warnRec map[string]any
	for _, r := range logs {
		switch r["level"] {
		case "ERROR":
			errRec = r
		case "WARN":
			warnRec = r
		}
	}
	if errRec == nil || errRec["format"] != "bracket" || errRec["level_raw"] != "ERROR" || errRec["msg"] != "disk full" {
		return fmt.Errorf("error record: %v", errRec)
	}
	if errRec["ts"] != "2026-08-16T12:00:00Z" || errRec["ts_source"] != "parsed" {
		return fmt.Errorf("error ts: %v %v", errRec["ts"], errRec["ts_source"])
	}
	if warnRec == nil || warnRec["msg"] != "slow query" || warnRec["ts_source"] != "ingest" {
		return fmt.Errorf("warn record: %v", warnRec)
	}
	return nil
}

func scenarioPlain() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		`something happened`,
		`[VERBOSE] hi`,
	}})
	if !eqFloat(body["ingested"], 2) {
		return fmt.Errorf("ingest: %v", body)
	}
	logs := logsList(mustGet(cl, "/logs"))
	if len(logs) != 2 {
		return fmt.Errorf("len=%d want 2", len(logs))
	}
	// Both are plain, level INFO, ts_source ingest. Find the VERBOSE one.
	var verboseRec map[string]any
	for _, r := range logs {
		if r["msg"] == "[VERBOSE] hi" {
			verboseRec = r
		}
	}
	if verboseRec == nil {
		return fmt.Errorf("verbose record missing: %v", logs)
	}
	if verboseRec["format"] != "plain" || verboseRec["level"] != "INFO" || verboseRec["level_raw"] != "" {
		return fmt.Errorf("verbose record: %v", verboseRec)
	}
	if verboseRec["ts_source"] != "ingest" {
		return fmt.Errorf("verbose ts_source=%v want ingest", verboseRec["ts_source"])
	}
	return nil
}

func scenarioJSONFallback() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		`{not valid json`,
	}})
	if !eqFloat(body["ingested"], 1) {
		return fmt.Errorf("ingest: %v (want graceful plain, not error)", body)
	}
	if errs, _ := body["errors"].([]any); len(errs) != 0 {
		return fmt.Errorf("unexpected errors: %v", errs)
	}
	logs := logsList(mustGet(cl, "/logs"))
	if len(logs) != 1 || logs[0]["format"] != "plain" || logs[0]["msg"] != "{not valid json" {
		return fmt.Errorf("fallback record: %v", logs)
	}
	return nil
}

func scenarioLevelAliases() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		`{"level":"WARNING","msg":"w"}`,
		`{"level":"ERR","msg":"e"}`,
		`{"level":"CRIT","msg":"c"}`,
		`{"level":"TRACE","msg":"t"}`,
		`{"level":"dbg","msg":"d"}`,
	}})
	if !eqFloat(body["ingested"], 5) {
		return fmt.Errorf("ingest: %v", body)
	}
	_, stats := cl.get("/stats")
	byLevel, _ := stats["by_level"].(map[string]any)
	want := map[string]float64{
		"DEBUG": 2, "INFO": 0, "WARN": 1, "ERROR": 1, "FATAL": 1,
	}
	for lvl, n := range want {
		if !eqFloat(byLevel[lvl], n) {
			return fmt.Errorf("by_level[%s]=%v want %v", lvl, byLevel[lvl], n)
		}
	}
	if !eqFloat(stats["total"], 5) {
		return fmt.Errorf("total=%v want 5", stats["total"])
	}
	return nil
}

func scenarioBadLevelRejected() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		`{"level":"VERBOSE","msg":"x"}`,
		`{"level":"info","msg":"y"}`,
	}})
	if !eqFloat(body["ingested"], 1) {
		return fmt.Errorf("ingested=%v want 1 (bad level rejected)", body["ingested"])
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 {
		return fmt.Errorf("errors=%v want 1", errs)
	}
	e0, _ := errs[0].(map[string]any)
	if !eqFloat(e0["line"], 1) {
		return fmt.Errorf("error line=%v want 1", e0["line"])
	}
	// Only the INFO record is stored.
	logs := logsList(mustGet(cl, "/logs"))
	if len(logs) != 1 || logs[0]["level"] != "INFO" || logs[0]["msg"] != "y" {
		return fmt.Errorf("logs=%v", logs)
	}
	return nil
}

func scenarioLineTooLong() error {
	cl := newClient()
	defer cl.close()
	long := strings.Repeat("a", lognorm.MaxLineLen+1) // 8193 chars
	_, body := cl.post("/ingest", map[string]any{"lines": []string{long}})
	if !eqFloat(body["ingested"], 0) {
		return fmt.Errorf("ingested=%v want 0", body["ingested"])
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 {
		return fmt.Errorf("errors=%v want 1", errs)
	}
	return nil
}

func scenarioBlankSkipped() error {
	cl := newClient()
	defer cl.close()
	_, body := cl.post("/ingest", map[string]any{"lines": []string{
		"", "   ", "hello", "\t\t",
	}})
	if !eqFloat(body["ingested"], 1) {
		return fmt.Errorf("ingested=%v want 1 (blanks skipped)", body["ingested"])
	}
	if errs, _ := body["errors"].([]any); len(errs) != 0 {
		return fmt.Errorf("errors=%v want none (blanks are not errors)", errs)
	}
	logs := logsList(mustGet(cl, "/logs"))
	if len(logs) != 1 || logs[0]["msg"] != "hello" || logs[0]["format"] != "plain" {
		return fmt.Errorf("logs=%v", logs)
	}
	return nil
}

func scenarioMinLevelFilter() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{
		`{"level":"debug","msg":"d"}`,
		`{"level":"info","msg":"i"}`,
		`{"level":"warn","msg":"w"}`,
		`{"level":"error","msg":"e"}`,
		`{"level":"fatal","msg":"f"}`,
	}})
	logs := logsList(mustGet(cl, "/logs?min_level=WARN"))
	if len(logs) != 3 {
		return fmt.Errorf("len=%d want 3", len(logs))
	}
	want := []string{"WARN", "ERROR", "FATAL"}
	for i, r := range logs {
		if r["level"] != want[i] {
			return fmt.Errorf("logs[%d].level=%v want %v", i, r["level"], want[i])
		}
	}
	// min_level is case-insensitive: lowercase works too.
	logs = logsList(mustGet(cl, "/logs?min_level=warn"))
	if len(logs) != 3 {
		return fmt.Errorf("lowercase min_level: len=%d want 3", len(logs))
	}
	return nil
}

func scenarioHalfOpenTime() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{
		`[INFO] 2026-08-16T10:00:00Z start`,
		`[INFO] 2026-08-16T10:30:00Z mid`,
		`[INFO] 2026-08-16T11:00:00Z end`,
	}})
	logs := logsList(mustGet(cl, "/logs?since=2026-08-16T10:00:00Z&until=2026-08-16T11:00:00Z"))
	if len(logs) != 2 {
		return fmt.Errorf("len=%d want 2 (start+mid, end excluded: until is exclusive)", len(logs))
	}
	if logs[0]["msg"] != "start" || logs[1]["msg"] != "mid" {
		return fmt.Errorf("msgs=%v,%v want start,mid", logs[0]["msg"], logs[1]["msg"])
	}
	// since is inclusive: the 10:00 record is present.
	// until is exclusive: the 11:00 record is absent even though ts == until.
	for _, r := range logs {
		if r["msg"] == "end" {
			return fmt.Errorf("end should be excluded (until exclusive)")
		}
	}
	return nil
}

func scenarioQFilter() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{
		`{"level":"info","msg":"foobar"}`,
		`{"level":"info","msg":"bazqux"}`,
	}})
	logs := logsList(mustGet(cl, "/logs?q=foo"))
	if len(logs) != 1 || logs[0]["msg"] != "foobar" {
		return fmt.Errorf("q=foo: %v", logs)
	}
	// case-sensitive: FOO does not match "foobar".
	logs = logsList(mustGet(cl, "/logs?q=FOO"))
	if len(logs) != 0 {
		return fmt.Errorf("q=FOO (case-sensitive): len=%d want 0", len(logs))
	}
	return nil
}

func scenarioFormatFilter() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{
		`{"level":"info","msg":"j"}`,
		`plain line`,
	}})
	logs := logsList(mustGet(cl, "/logs?format=json"))
	if len(logs) != 1 || logs[0]["format"] != "json" {
		return fmt.Errorf("format=json: %v", logs)
	}
	logs = logsList(mustGet(cl, "/logs?format=plain"))
	if len(logs) != 1 || logs[0]["format"] != "plain" {
		return fmt.Errorf("format=plain: %v", logs)
	}
	return nil
}

func scenarioSortOrder() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{
		`[INFO] 2026-08-16T10:30:00Z b`, // id log-000001
		`[INFO] 2026-08-16T10:00:00Z a`, // id log-000002
		`[INFO] 2026-08-16T10:00:00Z a2`, // id log-000003, same ts as a
	}})
	logs := logsList(mustGet(cl, "/logs"))
	if len(logs) != 3 {
		return fmt.Errorf("len=%d want 3", len(logs))
	}
	want := []string{"a", "a2", "b"} // ts asc, tie by id
	for i, r := range logs {
		if r["msg"] != want[i] {
			return fmt.Errorf("logs[%d].msg=%v want %v (id=%v)", i, r["msg"], want[i], r["id"])
		}
	}
	return nil
}

func scenarioStats() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{
		`{"level":"info","msg":"a"}`,
		`level=warn msg=b`,
		`[ERROR] boom`,
		`plain text`,
		`{"level":"info","msg":"c"}`,
	}})
	_, stats := cl.get("/stats")
	if !eqFloat(stats["total"], 5) {
		return fmt.Errorf("total=%v want 5", stats["total"])
	}
	byLevel, _ := stats["by_level"].(map[string]any)
	// INFO: a, c, and the plain line (plain defaults to INFO).
	if !eqFloat(byLevel["DEBUG"], 0) || !eqFloat(byLevel["INFO"], 3) || !eqFloat(byLevel["WARN"], 1) || !eqFloat(byLevel["ERROR"], 1) || !eqFloat(byLevel["FATAL"], 0) {
		return fmt.Errorf("by_level=%v", byLevel)
	}
	byFormat, _ := stats["by_format"].(map[string]any)
	if !eqFloat(byFormat["json"], 2) || !eqFloat(byFormat["logfmt"], 1) || !eqFloat(byFormat["bracket"], 1) || !eqFloat(byFormat["plain"], 1) {
		return fmt.Errorf("by_format=%v", byFormat)
	}
	return nil
}

func scenarioGetByID() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{`{"level":"info","msg":"x"}`}})
	code, body := cl.get("/logs/log-000001")
	if code != http.StatusOK || body["msg"] != "x" {
		return fmt.Errorf("get: code=%d body=%v", code, body)
	}
	if code, _ := cl.get("/logs/log-999999"); code != http.StatusNotFound {
		return fmt.Errorf("get missing: code=%d want 404", code)
	}
	return nil
}

func scenarioMinLevelInvalid() error {
	cl := newClient()
	defer cl.close()
	if code, _ := cl.get("/logs?min_level=FOO"); code != http.StatusBadRequest {
		return fmt.Errorf("min_level=FOO: code=%d want 400", code)
	}
	if code, _ := cl.get("/logs?min_level=verbose"); code != http.StatusBadRequest {
		return fmt.Errorf("min_level=verbose: code=%d want 400", code)
	}
	return nil
}

func scenarioTimeInvalid() error {
	cl := newClient()
	defer cl.close()
	if code, _ := cl.get("/logs?since=notatime"); code != http.StatusBadRequest {
		return fmt.Errorf("since=notatime: code=%d want 400", code)
	}
	if code, _ := cl.get("/logs?until=2026-13-40T00:00:00Z"); code != http.StatusBadRequest {
		return fmt.Errorf("until bad: code=%d want 400", code)
	}
	return nil
}

func scenarioLimit() error {
	cl := newClient()
	defer cl.close()
	cl.post("/ingest", map[string]any{"lines": []string{
		`{"level":"info","msg":"a"}`,
		`{"level":"info","msg":"b"}`,
		`{"level":"info","msg":"c"}`,
		`{"level":"info","msg":"d"}`,
		`{"level":"info","msg":"e"}`,
	}})
	_, body := cl.get("/logs?limit=2")
	logs := logsList(body)
	if len(logs) != 2 || !eqFloat(body["total"], 2) {
		return fmt.Errorf("limit=2: logs=%d total=%v want 2", len(logs), body["total"])
	}
	// limit above the cap is clamped to 1000, not rejected.
	if code, b := cl.get("/logs?limit=99999"); code != http.StatusOK {
		return fmt.Errorf("limit=99999: code=%d want 200 (clamped) body=%v", code, b)
	}
	return nil
}

func scenarioLimitInvalid() error {
	cl := newClient()
	defer cl.close()
	for _, v := range []string{"0", "-1", "abc", ""} {
		if v == "" {
			continue
		}
		if code, _ := cl.get("/logs?limit=" + v); code != http.StatusBadRequest {
			return fmt.Errorf("limit=%s: code=%d want 400", v, code)
		}
	}
	return nil
}

// mustGet fetches path and returns the decoded body, failing the scenario if
// the status is not 200.
func mustGet(cl *client, path string) map[string]any {
	code, body := cl.get(path)
	if code != http.StatusOK {
		panic(fmt.Sprintf("GET %s: code=%d body=%v", path, code, body))
	}
	return body
}
