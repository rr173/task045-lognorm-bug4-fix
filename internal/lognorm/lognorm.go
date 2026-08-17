// Package lognorm implements a log-line normalization and filtering service.
//
// It ingests heterogeneous raw log lines emitted by different upstream
// services — single-line JSON, logfmt (key=value), bracketed "[LEVEL] ts msg",
// or unstructured plain text — and normalizes each into a single canonical
// structured Record. Records are stored in memory and can be queried by level
// threshold, half-open time window, keyword and source format, or aggregated
// by level and format.
//
// Format detection follows a fixed precedence JSON → logfmt → bracket →
// plain. A line that looks like JSON but fails to parse degrades to plain
// rather than being rejected; only an explicit but unrecognizable level token
// (in JSON/logfmt) causes a line to be rejected, so bad levels never pollute
// downstream aggregation.
package lognorm

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sentinel errors returned by the service. The HTTP layer maps these to
// status codes.
var (
	ErrLinesRequired = errors.New("lines 不能为空")
	ErrTooManyLines  = errors.New("单次 ingest 行数超过上限")
	ErrLevelInvalid  = errors.New("无法识别的日志级别")
	ErrLineTooLong   = errors.New("单行超过长度上限")
)

// errNotMatch is an internal sentinel indicating a format detector did not
// match a line; the caller falls through to the next detector. It is never
// returned across the package boundary.
var errNotMatch = errors.New("format not matched")

// Canonical level constants and their total order (low -> high).
const (
	LevelDebug = "DEBUG"
	LevelInfo  = "INFO"
	LevelWarn  = "WARN"
	LevelError = "ERROR"
	LevelFatal = "FATAL"
)

// AllLevels is the canonical level set in total order (low -> high).
var AllLevels = []string{LevelDebug, LevelInfo, LevelWarn, LevelError, LevelFatal}

// AllFormats is the canonical source-format set, in detection order.
var AllFormats = []string{"json", "logfmt", "bracket", "plain"}

// levelOrder maps each canonical level to its position in the total order.
var levelOrder = map[string]int{
	LevelDebug: 0,
	LevelInfo:  1,
	LevelWarn:  2,
	LevelError: 3,
	LevelFatal: 4,
}

// aliasMap maps every recognized level alias (uppercased) to its canonical
// level. Matching is case- and surrounding-whitespace-insensitive.
var aliasMap = map[string]string{
	"DEBUG": LevelDebug, "DBG": LevelDebug, "TRACE": LevelDebug,
	"FINER": LevelDebug, "FINEST": LevelDebug, "FINE": LevelDebug,

	"INFO": LevelInfo, "INFORMATION": LevelInfo, "INFORM": LevelInfo,

	"WARN": LevelWarn, "WARNING": LevelWarn,

	"ERROR": LevelError, "ERR": LevelError,

	"FATAL": LevelFatal, "CRITICAL": LevelFatal, "CRIT": LevelFatal, "PANIC": LevelFatal,
}

// Key aliases for the structured fields, tried in order. Matching against
// input keys is case-insensitive.
var (
	tsKeys    = []string{"ts", "time", "timestamp", "@timestamp"}
	levelKeys = []string{"level", "lvl", "severity", "loglevel"}
	msgKeys   = []string{"msg", "message", "event"}
)

// tsLayouts tried in order when parsing a timestamp string. The first that
// parses the whole input wins; the result is always converted to UTC.
var tsLayouts = []string{
	time.RFC3339,            // 2026-08-16T10:00:00Z / +08:00, fractional ok
	"2006-01-02T15:04:05",   // no zone -> UTC
	"2006-01-02 15:04:05",   // space separator, no zone -> UTC
	"2006-01-02",            // date only -> 00:00:00 UTC
}

// MaxLineLen is the per-line character cap; longer lines are rejected.
const MaxLineLen = 8192

// MaxLines is the per-ingest line cap.
const MaxLines = 10000

// Record is the canonical normalized log entry.
type Record struct {
	ID       string         `json:"id"`
	TS       string         `json:"ts"`
	Level    string         `json:"level"`
	LevelRaw string         `json:"level_raw"`
	Msg      string         `json:"msg"`
	Format   string         `json:"format"`
	TSSource string         `json:"ts_source"`
	Fields   map[string]any `json:"fields"`

	// unexported: ingest sequence (stable sort tie-break) and parsed time
	// (filtering/sorting). Not serialized.
	seq    int
	tsTime time.Time
}

// IngestError describes a single rejected line.
type IngestError struct {
	Line  int    `json:"line"`  // 1-based index into the input lines
	Error string `json:"error"`
}

// IngestResult is the response of a batch ingest.
type IngestResult struct {
	Ingested int           `json:"ingested"`
	Errors   []IngestError `json:"errors"`
}

// Stats summarizes the stored records by level and format. ByLevel and
// ByFormat always contain every canonical level/format key (zero if absent).
type Stats struct {
	Total    int            `json:"total"`
	ByLevel  map[string]int `json:"by_level"`
	ByFormat map[string]int `json:"by_format"`
}

// Query holds the filters for a /logs lookup. Zero values mean "unfiltered"
// except Limit, whose default is applied by the caller.
type Query struct {
	MinLevel string    // canonical level; include this level and above
	Since    time.Time // include records with ts >= Since (inclusive)
	Until    time.Time // include records with ts < Until (exclusive)
	SinceSet bool      // whether Since is set
	UntilSet bool      // whether Until is set
	Q        string    // substring matched against Msg (case-sensitive)
	Format   string    // source format filter
	Limit    int       // cap on returned records (0 means caller's default)
}

// Service owns the in-memory log store.
type Service struct {
	mu      sync.Mutex
	records []*Record
	seq     int
}

// NewService creates an empty service.
func NewService() *Service { return &Service{} }

// Ingest normalizes each line and stores the valid ones. Blank lines are
// skipped silently; lines that are too long or carry an unrecognizable
// explicit level are reported in Errors and not stored. `now` is the ingest
// time used for records whose own timestamp is missing or unparseable.
func (s *Service) Ingest(lines []string, now time.Time) IngestResult {
	res := IngestResult{}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue // skip blank lines silently
		}
		if len(line) >= MaxLineLen {
			res.Errors = append(res.Errors, IngestError{Line: i + 1, Error: ErrLineTooLong.Error()})
			continue
		}
		rec, err := normalize(line, now)
		if err != nil {
			res.Errors = append(res.Errors, IngestError{Line: i + 1, Error: err.Error()})
			continue
		}
		s.seq++
		s.mu.Lock()
		rec.seq = s.seq
		rec.ID = fmt.Sprintf("log-%06d", s.seq)
		s.records = append(s.records, rec)
		s.mu.Unlock()
		res.Ingested++
	}
	return res
}

// Get returns the record with the given id, or (_, false) if not found.
func (s *Service) Get(id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.ID == id {
			return r, true
		}
	}
	return nil, false
}

// Query returns the records matching q, sorted by ts ascending with ingest
// order (id) as the tie-breaker, capped to q.Limit when positive.
func (s *Service) Query(q Query) []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()

	minOrd, hasMin := -1, false
	if q.MinLevel != "" {
		minOrd, hasMin = levelOrder[q.MinLevel]
	}

	var out []*Record
	for _, r := range s.records {
		if hasMin && levelOrder[r.Level] < minOrd {
			continue
		}
		if q.SinceSet && r.tsTime.Before(q.Since) {
			continue // ts < since
		}
		if q.UntilSet && !r.tsTime.Before(q.Until) {
			continue // ts >= until (until is exclusive)
		}
		if q.Q != "" && !strings.Contains(r.Msg, q.Q) {
			continue
		}
		if q.Format != "" && r.Format != q.Format {
			continue
		}
		out = append(out, r)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].tsTime.Equal(out[j].tsTime) {
			return out[i].tsTime.Before(out[j].tsTime)
		}
		return out[i].seq < out[j].seq
	})

	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// Stats returns aggregate counts over all stored records.
func (s *Service) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		ByLevel:  make(map[string]int, len(AllLevels)),
		ByFormat: make(map[string]int, len(AllFormats)),
	}
	for _, lvl := range AllLevels {
		st.ByLevel[lvl] = 0
	}
	for _, f := range AllFormats {
		st.ByFormat[f] = 0
	}
	for _, r := range s.records {
		st.ByLevel[r.Level]++
		st.ByFormat[r.Format]++
	}
	st.Total = len(s.records)
	return st
}

// IsValidLevel reports whether v is a canonical level string.
func IsValidLevel(v string) bool {
	_, ok := levelOrder[v]
	return ok
}

// IsValidFormat reports whether v is a recognized source format.
func IsValidFormat(v string) bool {
	for _, f := range AllFormats {
		if f == v {
			return true
		}
	}
	return false
}

// LevelRank returns the total-order position of a canonical level, or (-1,
// false) if v is not canonical.
func LevelRank(v string) (int, bool) {
	r, ok := levelOrder[v]
	return r, ok
}

// normalize parses one raw line into a Record, or returns an error if the
// line carries an explicit but unrecognizable level.
func normalize(line string, now time.Time) (*Record, error) {
	trimmed := strings.TrimSpace(line)

	// 1. JSON: leading '{' and parses as an object.
	if strings.HasPrefix(trimmed, "{") {
		rec, err := tryJSON(trimmed, now)
		if err == nil {
			return rec, nil
		}
		if err != errNotMatch {
			return nil, err // unrecognizable explicit level
		}
		// else: fall through (looked like JSON but didn't parse)
	}

	// 2. logfmt: first quote-aware token is key=value.
	if rec, err := tryLogfmt(trimmed, now); err == nil {
		return rec, nil
	} else if err != errNotMatch {
		return nil, err
	}

	// 3. bracket: leading "[ALIAS]" with a recognizable level.
	if strings.HasPrefix(trimmed, "[") {
		if rec, err := tryBracket(trimmed, now); err == nil {
			return rec, nil
		} else if err != errNotMatch {
			return nil, err
		}
	}

	// 4. plain fallback.
	return plainRecord(trimmed, now), nil
}

// tryJSON parses a JSON-object log line. Returns errNotMatch if the line is
// not valid JSON, or ErrLevelInvalid if it carries an unrecognizable level.
func tryJSON(trimmed string, now time.Time) (*Record, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil, errNotMatch
	}

	rec := &Record{
		Format:   "json",
		Fields:   map[string]any{},
		Level:    LevelInfo,
		LevelRaw: "",
		TS:       now.Format(time.RFC3339),
		TSSource: "ingest",
		tsTime:   now,
	}

	consumed := map[string]bool{}
	if tsVal, _ := consumeAlias(m, tsKeys, consumed); tsVal != "" {
		if t, ok := parseTS(tsVal); ok {
			rec.tsTime = t
			rec.TS = t.UTC().Format(time.RFC3339)
			rec.TSSource = "parsed"
		}
	}
	levelVal, levelFound := consumeAlias(m, levelKeys, consumed)
	if msgVal, _ := consumeAlias(m, msgKeys, consumed); msgVal != "" {
		rec.Msg = msgVal
	}
	for k, v := range m {
		if !consumed[k] {
			rec.Fields[k] = v
		}
	}

	if levelFound && levelVal != "" {
		lvl, ok := canonicalLevel(levelVal)
		if !ok {
			return nil, ErrLevelInvalid
		}
		rec.Level = lvl
		rec.LevelRaw = levelVal
	}
	return rec, nil
}

// consumeAlias finds the value of the first-present alias (case-insensitive)
// in m and marks ALL alias-matching keys as consumed. It returns the value
// (only when the matching key holds a string) and whether such a string-valued
// alias key was found.
func consumeAlias(m map[string]any, aliases []string, consumed map[string]bool) (string, bool) {
	var val string
	found := false
	for _, a := range aliases {
		for mk, mv := range m {
			if strings.EqualFold(mk, a) {
				consumed[mk] = true
				if !found {
					if s, ok := toString(mv); ok {
						val = s
						found = true
					}
				}
			}
		}
	}
	return val, found
}

// kv is one parsed logfmt token.
type kv struct {
	key  string
	val  string
	bare bool // true when no '=' followed the key
}

// parseLogfmt tokenizes a logfmt line, honoring double-quoted values (with
// \" and \\ escapes) and spaces inside quotes. It returns ok=false when the
// first token is not of the form key=value (the heuristic that distinguishes
// logfmt from plain text).
func parseLogfmt(s string) ([]kv, bool) {
	var toks []kv
	i, n := 0, len(s)
	first := true
	for i < n {
		// skip whitespace
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		// read key (bare identifier up to '=' or whitespace)
		start := i
		for i < n && s[i] != '=' && s[i] != ' ' && s[i] != '\t' {
			i++
		}
		key := s[start:i]
		if i < n && s[i] == '=' {
			i++ // skip '='
			var val string
			if i < n && s[i] == '"' {
				i++ // opening quote
				var b strings.Builder
				for i < n {
					if s[i] == '\\' && i+1 < n {
						next := s[i+1]
						if next == '"' || next == '\\' {
							b.WriteByte(next)
							i += 2
							continue
						}
						b.WriteByte('\\') // unknown escape: keep literally
						i++
						continue
					}
					if s[i] == '"' {
						i++
						break
					}
					b.WriteByte(s[i])
					i++
				}
				val = b.String()
			} else {
				vstart := i
				for i < n && s[i] != ' ' && s[i] != '\t' {
					i++
				}
				val = s[vstart:i]
			}
			toks = append(toks, kv{key: key, val: val})
			first = false
		} else {
			// bare token (no '=')
			if first {
				return nil, false
			}
			toks = append(toks, kv{key: key, bare: true})
		}
		first = false
	}
	if len(toks) == 0 {
		return nil, false
	}
	return toks, true
}

// tryLogfmt parses a logfmt line. Returns errNotMatch if the first token is
// not key=value, or ErrLevelInvalid if an explicit level is unrecognizable.
func tryLogfmt(trimmed string, now time.Time) (*Record, error) {
	toks, ok := parseLogfmt(trimmed)
	if !ok {
		return nil, errNotMatch
	}

	rec := &Record{
		Format:   "logfmt",
		Fields:   map[string]any{},
		Level:    LevelInfo,
		LevelRaw: "",
		TS:       now.Format(time.RFC3339),
		TSSource: "ingest",
		tsTime:   now,
	}

	var levelVal string
	levelPresent := false
	for _, t := range toks {
		lk := strings.ToLower(t.key)
		switch {
		case contains(tsKeys, lk):
			if !t.bare && t.val != "" {
				if tt, ok := parseTS(t.val); ok {
					rec.tsTime = tt
					rec.TS = tt.UTC().Format(time.RFC3339)
					rec.TSSource = "parsed"
				}
			}
		case contains(levelKeys, lk):
			if !t.bare && t.val != "" {
				levelVal = t.val
				levelPresent = true
			}
		case contains(msgKeys, lk):
			if !t.bare {
				rec.Msg = t.val
			}
		default:
			if t.bare {
				rec.Fields[t.key] = true
			} else {
				rec.Fields[t.key] = t.val
			}
		}
	}

	if levelPresent {
		lvl, ok := canonicalLevel(levelVal)
		if !ok {
			return nil, ErrLevelInvalid
		}
		rec.Level = lvl
		rec.LevelRaw = levelVal
	}
	return rec, nil
}

// tryBracket parses a "[LEVEL] <optional ts> <message>" line. Returns
// errNotMatch when the leading bracket content is not a recognizable level
// (the line then falls through to plain).
func tryBracket(trimmed string, now time.Time) (*Record, error) {
	if !strings.HasPrefix(trimmed, "[") {
		return nil, errNotMatch
	}
	idx := strings.Index(trimmed, "]")
	if idx < 0 {
		return nil, errNotMatch
	}
	content := trimmed[1:idx]
	lvl, ok := canonicalLevel(content)
	if !ok {
		return nil, errNotMatch // not a level marker -> plain
	}

	rec := &Record{
		Format:   "bracket",
		Fields:   map[string]any{},
		Level:    lvl,
		LevelRaw: strings.TrimSpace(content),
		TS:       now.Format(time.RFC3339),
		TSSource: "ingest",
		tsTime:   now,
	}

	rest := strings.TrimSpace(trimmed[idx+1:])
	if rest == "" {
		rec.Msg = ""
		return rec, nil
	}
	// Try the first whitespace-delimited token as a timestamp.
	sp := strings.IndexAny(rest, " \t")
	var firstTok, remainder string
	if sp < 0 {
		firstTok = rest
		remainder = ""
	} else {
		firstTok = rest[:sp]
		remainder = strings.TrimSpace(rest[sp+1:])
	}
	if t, ok := parseTS(firstTok); ok {
		rec.tsTime = t
		rec.TS = t.UTC().Format(time.RFC3339)
		rec.TSSource = "parsed"
		rec.Msg = remainder
	} else {
		rec.Msg = rest
	}
	return rec, nil
}

// plainRecord builds the fallback record for an unstructured line.
func plainRecord(trimmed string, now time.Time) *Record {
	return &Record{
		Format:   "plain",
		Fields:   map[string]any{},
		Level:    LevelInfo,
		LevelRaw: "",
		Msg:      trimmed,
		TS:       now.Format(time.RFC3339),
		TSSource: "ingest",
		tsTime:   now,
	}
}

// canonicalLevel maps a raw level token to its canonical level. Matching is
// case- and surrounding-whitespace-insensitive.
func canonicalLevel(raw string) (string, bool) {
	up := strings.ToUpper(strings.TrimSpace(raw))
	lvl, ok := aliasMap[up]
	return lvl, ok
}

// parseTS parses a timestamp string using tsLayouts, returning the time in
// UTC. Returns (_, false) if no layout matches the whole input.
func parseTS(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range tsLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// toString returns v as a string when it is a JSON string.
func toString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// contains reports whether s is present in ss.
func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
