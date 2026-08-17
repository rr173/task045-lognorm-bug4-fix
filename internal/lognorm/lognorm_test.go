package lognorm

import (
	"errors"
	"testing"
	"time"
)

// fixedNow is a stable ingest time for deterministic tests.
var fixedNow = time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)

func TestCanonicalLevel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"info", LevelInfo}, {"INFO", LevelInfo}, {"Information", LevelInfo},
		{"warn", LevelWarn}, {"WARNING", LevelWarn},
		{"err", LevelError}, {"ERROR", LevelError},
		{"fatal", LevelFatal}, {"CRIT", LevelFatal}, {"critical", LevelFatal}, {"panic", LevelFatal},
		{"debug", LevelDebug}, {"DBG", LevelDebug}, {"trace", LevelDebug}, {"finer", LevelDebug},
		{"  warn  ", LevelWarn}, // surrounding whitespace
	} {
		got, ok := canonicalLevel(tc.in)
		if !ok || got != tc.want {
			t.Errorf("canonicalLevel(%q)=%q,%v want %q,true", tc.in, got, ok, tc.want)
		}
	}
}

func TestCanonicalLevel_Unknown(t *testing.T) {
	for _, in := range []string{"VERBOSE", "CUSTOM", "x", "", "123"} {
		if _, ok := canonicalLevel(in); ok {
			t.Errorf("canonicalLevel(%q): want false", in)
		}
	}
}

func TestParseTS_Layouts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"2026-08-16T10:00:00Z", "2026-08-16T10:00:00Z"},
		{"2026-08-16T10:00:00+08:00", "2026-08-16T02:00:00Z"}, // offset to UTC
		{"2026-08-16T10:00:00.123Z", "2026-08-16T10:00:00Z"},     // fractional dropped
		{"2026-08-16T10:00:00", "2026-08-16T10:00:00Z"},         // no zone -> UTC
		{"2026-08-16 10:00:00", "2026-08-16T10:00:00Z"},          // space sep
		{"2026-08-16", "2026-08-16T00:00:00Z"},                  // date only
	} {
		got, ok := parseTS(tc.in)
		if !ok {
			t.Fatalf("parseTS(%q): not ok", tc.in)
		}
		if got.UTC().Format(time.RFC3339) != tc.want {
			t.Errorf("parseTS(%q)=%v want %s", tc.in, got.Format(time.RFC3339), tc.want)
		}
	}
}

func TestParseTS_Invalid(t *testing.T) {
	for _, in := range []string{"", "not a time", "2026/08/16", "2026-13-40"} {
		if _, ok := parseTS(in); ok {
			t.Errorf("parseTS(%q): want false", in)
		}
	}
}

func TestNormalize_JSON(t *testing.T) {
	rec, err := normalize(`{"ts":"2026-08-16T10:00:00Z","level":"info","msg":"hi","app":"api","status":200}`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Format != "json" || rec.Level != LevelInfo || rec.LevelRaw != "info" || rec.Msg != "hi" {
		t.Errorf("json rec: %+v", rec)
	}
	if rec.TS != "2026-08-16T10:00:00Z" || rec.TSSource != "parsed" {
		t.Errorf("json ts: %q %q", rec.TS, rec.TSSource)
	}
	if rec.Fields["app"] != "api" {
		t.Errorf("fields app=%v", rec.Fields["app"])
	}
	// JSON number decodes to float64 in the client; here the value is the raw
	// json.Unmarshal result (float64).
	if v, ok := rec.Fields["status"].(float64); !ok || v != 200 {
		t.Errorf("fields status=%v type %T", rec.Fields["status"], rec.Fields["status"])
	}
}

func TestNormalize_JSONAliasKeys(t *testing.T) {
	rec, err := normalize(`{"time":"2026-08-16T10:00:00Z","lvl":"warn","message":"hi","@timestamp":"ignored","severity":"error"}`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Level != LevelWarn {
		t.Errorf("level=%v want WARN (lvl wins over severity)", rec.Level)
	}
	if rec.Msg != "hi" {
		t.Errorf("msg=%v", rec.Msg)
	}
	if rec.TS != "2026-08-16T10:00:00Z" || rec.TSSource != "parsed" {
		t.Errorf("ts=%v src=%v", rec.TS, rec.TSSource)
	}
}

func TestNormalize_JSONBadLevel(t *testing.T) {
	_, err := normalize(`{"level":"VERBOSE","msg":"x"}`, fixedNow)
	if !errors.Is(err, ErrLevelInvalid) {
		t.Errorf("err=%v want ErrLevelInvalid", err)
	}
}

func TestNormalize_JSONFallbackToPlain(t *testing.T) {
	rec, err := normalize(`{not valid json`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Format != "plain" {
		t.Errorf("format=%v want plain", rec.Format)
	}
	if rec.Msg != "{not valid json" {
		t.Errorf("msg=%q", rec.Msg)
	}
	if rec.Level != LevelInfo || rec.LevelRaw != "" {
		t.Errorf("level=%v raw=%v", rec.Level, rec.LevelRaw)
	}
}

func TestNormalize_Logfmt(t *testing.T) {
	rec, err := normalize(`level=info msg="hello world" status=200 app=api`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Format != "logfmt" || rec.Level != LevelInfo || rec.Msg != "hello world" {
		t.Errorf("logfmt rec: %+v", rec)
	}
	if rec.Fields["status"] != "200" || rec.Fields["app"] != "api" {
		t.Errorf("fields=%v", rec.Fields)
	}
}

func TestNormalize_LogfmtQuotedEscape(t *testing.T) {
	rec, err := normalize(`level=warn msg="she said \"hi\""`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Msg != `she said "hi"` {
		t.Errorf("msg=%q", rec.Msg)
	}
}

func TestNormalize_LogfmtBareToken(t *testing.T) {
	rec, err := normalize(`level=info started`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if v, ok := rec.Fields["started"].(bool); !ok || !v {
		t.Errorf("bare token started=%v want true", rec.Fields["started"])
	}
}

func TestNormalize_LogfmtBadLevel(t *testing.T) {
	_, err := normalize(`level=VERBOSE msg=x`, fixedNow)
	if !errors.Is(err, ErrLevelInvalid) {
		t.Errorf("err=%v want ErrLevelInvalid", err)
	}
}

func TestNormalize_Plain(t *testing.T) {
	rec, err := normalize(`something happened`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Format != "plain" || rec.Level != LevelInfo || rec.LevelRaw != "" {
		t.Errorf("plain rec: %+v", rec)
	}
	if rec.Msg != "something happened" {
		t.Errorf("msg=%q", rec.Msg)
	}
	if rec.TSSource != "ingest" || rec.TS != fixedNow.Format(time.RFC3339) {
		t.Errorf("ts=%v src=%v", rec.TS, rec.TSSource)
	}
}

func TestNormalize_BracketWithTS(t *testing.T) {
	rec, err := normalize(`[ERROR] 2026-08-16T12:00:00Z disk full`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Format != "bracket" || rec.Level != LevelError || rec.LevelRaw != "ERROR" {
		t.Errorf("bracket rec: %+v", rec)
	}
	if rec.TS != "2026-08-16T12:00:00Z" || rec.TSSource != "parsed" {
		t.Errorf("ts=%v src=%v", rec.TS, rec.TSSource)
	}
	if rec.Msg != "disk full" {
		t.Errorf("msg=%q", rec.Msg)
	}
}

func TestNormalize_BracketNoTS(t *testing.T) {
	rec, err := normalize(`[WARN] slow query`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Format != "bracket" || rec.Level != LevelWarn {
		t.Errorf("bracket rec: %+v", rec)
	}
	if rec.TSSource != "ingest" {
		t.Errorf("ts_source=%v want ingest", rec.TSSource)
	}
	if rec.Msg != "slow query" {
		t.Errorf("msg=%q", rec.Msg)
	}
}

func TestNormalize_BracketNonLevelFallsToPlain(t *testing.T) {
	rec, err := normalize(`[VERBOSE] hi`, fixedNow)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.Format != "plain" {
		t.Errorf("format=%v want plain", rec.Format)
	}
	if rec.Msg != "[VERBOSE] hi" {
		t.Errorf("msg=%q", rec.Msg)
	}
	if rec.Level != LevelInfo {
		t.Errorf("level=%v want INFO", rec.Level)
	}
}

func TestParseLogfmt_FirstTokenMustBeKV(t *testing.T) {
	// A plain sentence has no key=value first token -> not logfmt.
	if _, ok := parseLogfmt("hello world"); ok {
		t.Error("hello world should not be logfmt")
	}
	if _, ok := parseLogfmt("level=info msg=hi"); !ok {
		t.Error("level=info msg=hi should be logfmt")
	}
}

func TestService_Ingest_BlankAndErrors(t *testing.T) {
	s := NewService()
	long := stringRepeat("a", MaxLineLen+1)
	res := s.Ingest([]string{"", "   ", "hello", long, `{"level":"BAD","msg":"x"}`, `{"level":"info","msg":"y"}`}, fixedNow)
	if res.Ingested != 2 {
		t.Errorf("ingested=%d want 2", res.Ingested)
	}
	if len(res.Errors) != 2 {
		t.Errorf("errors=%d want 2", len(res.Errors))
	}
	// Errors: line 4 (too long), line 5 (bad level). Line 6 ingested.
	if res.Errors[0].Line != 4 || res.Errors[1].Line != 5 {
		t.Errorf("error lines=%v want [4,5]", res.Errors)
	}
}

func TestService_Query_MinLevel(t *testing.T) {
	s := NewService()
	s.Ingest([]string{
		`{"level":"debug","msg":"d"}`,
		`{"level":"info","msg":"i"}`,
		`{"level":"warn","msg":"w"}`,
		`{"level":"error","msg":"e"}`,
		`{"level":"fatal","msg":"f"}`,
	}, fixedNow)
	recs := s.Query(Query{MinLevel: LevelWarn, Limit: 100})
	if len(recs) != 3 {
		t.Fatalf("len=%d want 3", len(recs))
	}
	got := []string{recs[0].Level, recs[1].Level, recs[2].Level}
	want := []string{LevelWarn, LevelError, LevelFatal}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("rec[%d]=%v want %v", i, got[i], want[i])
		}
	}
}

func TestService_Query_HalfOpenTime(t *testing.T) {
	s := NewService()
	s.Ingest([]string{
		`[INFO] 2026-08-16T10:00:00Z start`,
		`[INFO] 2026-08-16T10:30:00Z mid`,
		`[INFO] 2026-08-16T11:00:00Z end`,
	}, fixedNow)
	since, _ := parseTS("2026-08-16T10:00:00Z")
	until, _ := parseTS("2026-08-16T11:00:00Z")
	recs := s.Query(Query{Since: since, SinceSet: true, Until: until, UntilSet: true, Limit: 100})
	if len(recs) != 2 {
		t.Fatalf("len=%d want 2 (start+mid, not end)", len(recs))
	}
	if recs[0].Msg != "start" || recs[1].Msg != "mid" {
		t.Errorf("msgs=%q,%q", recs[0].Msg, recs[1].Msg)
	}
}

func TestService_Query_SortOrder(t *testing.T) {
	s := NewService()
	s.Ingest([]string{
		`[INFO] 2026-08-16T10:30:00Z b`, // seq 1
		`[INFO] 2026-08-16T10:00:00Z a`, // seq 2
		`[INFO] 2026-08-16T10:00:00Z a2`, // seq 3, same ts as a
	}, fixedNow)
	recs := s.Query(Query{Limit: 100})
	if len(recs) != 3 {
		t.Fatalf("len=%d", len(recs))
	}
	want := []string{"a", "a2", "b"} // by ts asc, tie by id
	for i, r := range recs {
		if r.Msg != want[i] {
			t.Errorf("rec[%d]=%q want %q (id=%s)", i, r.Msg, want[i], r.ID)
		}
	}
}

func TestService_Query_QAndFormat(t *testing.T) {
	s := NewService()
	s.Ingest([]string{
		`{"level":"info","msg":"foobar"}`,
		`{"level":"info","msg":"baz"}`,
		`level=info msg=keepme`,
	}, fixedNow)
	recs := s.Query(Query{Q: "foo", Limit: 100})
	if len(recs) != 1 || recs[0].Msg != "foobar" {
		t.Errorf("q=foo: %v", recs)
	}
	recs = s.Query(Query{Format: "logfmt", Limit: 100})
	if len(recs) != 1 || recs[0].Format != "logfmt" {
		t.Errorf("format=logfmt: %v", recs)
	}
}

func TestService_Stats(t *testing.T) {
	s := NewService()
	s.Ingest([]string{
		`{"level":"info","msg":"a"}`,
		`level=warn msg=b`,
		`[ERROR] boom`,
		`plain text`, // plain defaults to INFO
		`{"level":"info","msg":"c"}`,
	}, fixedNow)
	st := s.Stats()
	if st.Total != 5 {
		t.Errorf("total=%d want 5", st.Total)
	}
	// INFO: a, c, and the plain line (plain -> INFO by default).
	if st.ByLevel[LevelInfo] != 3 || st.ByLevel[LevelWarn] != 1 || st.ByLevel[LevelError] != 1 || st.ByLevel[LevelDebug] != 0 || st.ByLevel[LevelFatal] != 0 {
		t.Errorf("by_level=%v", st.ByLevel)
	}
	if st.ByFormat["json"] != 2 || st.ByFormat["logfmt"] != 1 || st.ByFormat["bracket"] != 1 || st.ByFormat["plain"] != 1 {
		t.Errorf("by_format=%v", st.ByFormat)
	}
}

func TestService_Get(t *testing.T) {
	s := NewService()
	s.Ingest([]string{`{"level":"info","msg":"x"}`}, fixedNow)
	rec, ok := s.Get("log-000001")
	if !ok || rec.Msg != "x" {
		t.Errorf("get: ok=%v rec=%+v", ok, rec)
	}
	if _, ok := s.Get("log-999999"); ok {
		t.Error("get missing: want false")
	}
}

// stringRepeat builds a string of n copies of s (avoids a strings import
// cycle concern in the test file's own helpers; strings is already imported
// by the package, not the test, so we use it directly).
func stringRepeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}
