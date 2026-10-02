package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openclaw/crawlkit/output"

	"github.com/ourostack/teamscrawl/internal/errs"
)

// meta is embedded last in every read result so its keys come after the command's own.
type meta struct {
	ArchiveAgeSeconds *int64 `json:"archive_age_seconds"`
	// Synced is present only when this read ran the implicit sync first (--max-age): how long the
	// sync took and how it ended. Absent when the archive was fresh enough to read as it was.
	Synced    *syncedInfo `json:"synced,omitempty"`
	SyncError *syncError  `json:"sync_error,omitempty"`
	// NeedsSync and Hint tell an agent in-band that the archive has never synced, so an empty
	// result means "no data yet", not "nothing matched". Omitted once a sync has succeeded.
	NeedsSync bool   `json:"needs_sync,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

// syncError is the implicit sync's failure, reported beside a result that was still served.
type syncError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// syncedInfo reports the implicit sync a read ran before answering.
type syncedInfo struct {
	Seconds float64 `json:"seconds"` // how long the sync took, to a tenth of a second
	Status  string  `json:"status"`  // the sync report's status: ok, ok_with_omissions, unchanged, partial or failed
}

func (m *meta) setSynced(si *syncedInfo) { m.Synced = si }

func (m *meta) setMeta(age *int64, se *syncError) { m.ArchiveAgeSeconds, m.SyncError = age, se }

func (m *meta) setNeedsSync(hint string) { m.NeedsSync, m.Hint = true, hint }

type result interface {
	setMeta(*int64, *syncError)
	setSynced(*syncedInfo)
	setNeedsSync(hint string)
}

// listResult is the shape of every list command.
type listResult struct {
	Items     []any `json:"items"`
	Count     int   `json:"count"`
	Truncated bool  `json:"truncated"`
	// Total is the exact number of matches ignoring --limit. It is present only when truncated is
	// true: counting costs one extra query, and a result that is not truncated is its own total
	// (count).
	Total int `json:"total,omitempty"`
	// ChannelsExcluded says channels were left out (--include-channels not set) so an agent
	// does not read the result as covering every conversation. Omitted otherwise.
	ChannelsExcluded bool `json:"channels_excluded,omitempty"`
	meta
}

func newList(items []any, truncated bool) *listResult {
	if items == nil {
		items = []any{}
	}
	return &listResult{Items: items, Count: len(items), Truncated: truncated}
}

// withTotal records total on a truncated list and leaves any other list as it is.
func (l *listResult) withTotal(total int) *listResult {
	if l.Truncated {
		l.Total = total
	}
	return l
}

// guessFormat picks the error format before flags are validated, by looking at the raw args.
func guessFormat(args []string, tty bool) output.Format {
	for i, a := range args {
		switch {
		case a == "--json", a == "--format=json":
			return output.JSON
		case a == "--format" && i+1 < len(args):
			if f := args[i+1]; f == "text" || f == "json" || f == "log" {
				return output.Format(f)
			}
		}
	}
	if tty {
		return output.Text
	}
	return output.JSON
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// write prints one result to stdout in the chosen format.
func (rt *runtime) write(label string, v any) error {
	if rt.format == output.Text {
		return rt.renderText(label, v)
	}
	if rt.format == output.JSON {
		// Compact for pipes (agents pay per token), indented on a terminal; links keep their & intact.
		enc := json.NewEncoder(rt.stdout)
		enc.SetEscapeHTML(false)
		if rt.stdoutTTY {
			enc.SetIndent("", "  ")
		}
		return enc.Encode(v)
	}
	return output.Write(rt.stdout, rt.format, label, v)
}

type errorDoc struct {
	Error errorBody `json:"error"`
}

type warningDoc struct {
	Warning errorBody `json:"warning"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

func bodyOf(c *errs.Coded) errorBody {
	msg := c.Message
	if c.Code == errs.CodeDBError && c.Unwrap() != nil {
		msg += ": " + c.Unwrap().Error()
	}
	return errorBody{Code: c.Code, Message: msg, Fix: outputFix(c)}
}

// printError writes a coded error to stderr: one JSON line in JSON/log mode, or a plain line
// plus a "fix:" line in text mode.
func (rt *runtime) printError(c *errs.Coded) {
	b := bodyOf(c)
	if rt.format == output.Text {
		_, _ = fmt.Fprintf(rt.stderr, "error: %s\nfix: %s\n", b.Message, b.Fix)
		return
	}
	rt.writeJSONLine(errorDoc{Error: b})
}

// printWarning reports a problem that did not stop the command (a failed implicit sync).
func (rt *runtime) printWarning(c *errs.Coded) {
	b := bodyOf(c)
	if rt.format == output.Text {
		_, _ = fmt.Fprintf(rt.stderr, "warning: %s\nfix: %s\n", b.Message, b.Fix)
		return
	}
	rt.writeJSONLine(warningDoc{Warning: b})
}

// writeJSONLine writes one compact JSON line to stderr with the same encoder settings as results:
// fix text such as <tenantId>/<userId> keeps its < and > instead of \u003c.
func (rt *runtime) writeJSONLine(v any) {
	enc := json.NewEncoder(rt.stderr)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (rt *runtime) hint(msg string) { _, _ = fmt.Fprintf(rt.stderr, "hint: %s\n", msg) }

// jsonKeys lists a struct type's JSON keys in field order.
func jsonKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
	}
	return keys
}

// projected is an item reduced to the keys --fields asked for, in the order asked.
type projected struct {
	keys []string
	vals map[string]json.RawMessage
}

func (p projected) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range p.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(p.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func project(item any, fields []string) (any, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // same bytes as the unprojected path: links keep their &
	if err := enc.Encode(item); err != nil {
		return nil, err
	}
	raw := buf.Bytes()
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	p := projected{vals: all}
	for _, f := range fields {
		if _, ok := all[f]; ok {
			p.keys = append(p.keys, f)
		}
	}
	if _, ok := all["text_truncated"]; ok && contains(fields, "text") {
		p.keys = append(p.keys, "text_truncated")
	}
	return p, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// shape applies --fields to typed items. Every T is one of this package's own item structs, which
// always encode, so a projection failure is a programming error: it panics with a plain error and runCLI
// reports it as an internal error.
func shape[T any](rt *runtime, in []T) []any {
	out := make([]any, len(in))
	for i, it := range in {
		if len(rt.fields) == 0 {
			out[i] = it
			continue
		}
		p, err := project(it, rt.fields)
		if err != nil {
			panic(fmt.Errorf("cannot project an item onto --fields: %w", err))
		}
		out[i] = p
	}
	return out
}

// checkFields rejects --fields keys that item type T does not have.
func checkFields[T any](rt *runtime) error {
	if len(rt.fields) == 0 {
		return nil
	}
	valid := jsonKeys(reflect.TypeFor[T]())
	valid = removeKey(valid, "text_truncated")
	for _, f := range rt.fields {
		if !contains(valid, f) {
			c := errs.Usage(fmt.Sprintf("unknown --fields key %q; valid keys: %s", f, strings.Join(valid, ", ")))
			c.Fix = "Pick keys from the list in the message."
			return c
		}
	}
	return nil
}

func removeKey(ss []string, k string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != k {
			out = append(out, s)
		}
	}
	return out
}

func splitFields(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// truncateRunes cuts s to max runes and appends an ellipsis; max <= 0 disables truncation.
func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s, false
	}
	r := []rune(s)
	return string(r[:max]) + "…", true
}

var relativeDur = regexp.MustCompile(`^(\d+(?:\.\d+)?)([dw])$`)

// parseDuration reads a Go duration, or a number of days (7d) or weeks (2w).
func parseDuration(s string) (time.Duration, error) {
	if m := relativeDur.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, err
		}
		unit := 24 * time.Hour
		if m[2] == "w" {
			unit = 7 * 24 * time.Hour
		}
		return time.Duration(n * float64(unit)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q is negative", s)
	}
	return d, nil
}

// parseWhen reads an RFC3339 time, a YYYY-MM-DD date (midnight in loc) or a relative duration
// back from now (90m, 24h, 7d, 2w).
func parseWhen(s string, now time.Time, loc *time.Location) (time.Time, error) {
	bad := func() (time.Time, error) {
		return time.Time{}, fmt.Errorf("cannot read %q as a time: use RFC3339, YYYY-MM-DD or a relative duration such as 90m, 24h, 7d, 2w", s)
	}
	if s == "" {
		return bad()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t.UTC(), nil
	}
	d, err := parseDuration(s)
	if err != nil || d <= 0 {
		return bad()
	}
	return now.Add(-d).UTC(), nil
}

// parseMaxAge reads --max-age; 0 disables the implicit sync.
func parseMaxAge(s string) (time.Duration, error) {
	d, err := parseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--max-age %q: use a duration such as 15m, 2h, 1d, or 0 to disable", s)
	}
	return d, nil
}
