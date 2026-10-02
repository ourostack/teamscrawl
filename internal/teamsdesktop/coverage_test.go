package teamsdesktop

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/errs"
	"github.com/ourostack/teamscrawl/internal/indexeddb"
	"github.com/ourostack/teamscrawl/internal/leveldb"
	"github.com/ourostack/teamscrawl/internal/v8"
)

func TestDefaultRootWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("LOCALAPPDATA", "")
	r := DefaultRoot()
	switch goruntime.GOOS {
	case "windows":
		if !strings.HasPrefix(r, filepath.Join("~", "AppData", "Local")) {
			t.Fatalf("DefaultRoot without a home = %q", r)
		}
	default:
		if !strings.HasPrefix(r, "~") {
			t.Fatalf("DefaultRoot without a home = %q, want it to start with ~", r)
		}
	}
}

func TestDiscoverSkipsFilesAndProfilesWithoutIndexedDB(t *testing.T) {
	root := fakeTree(t, "Zeta", "https_teams.microsoft.com_0")
	if err := os.WriteFile(filepath.Join(root, "Local State"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Crashpad"), 0o700); err != nil { // a directory with no IndexedDB
		t.Fatal(err)
	}
	// A second profile sorts before the first: Discover returns them ordered by key.
	if err := os.MkdirAll(filepath.Join(root, "Alpha", "IndexedDB", "https_teams.microsoft.com_0.indexeddb.leveldb"), 0o700); err != nil {
		t.Fatal(err)
	}
	srcs, _, err := Discover(root)
	if err != nil || len(srcs) != 2 || srcs[0].Profile != "Alpha" || srcs[1].Profile != "Zeta" {
		t.Fatalf("sources = %+v, %v", srcs, err)
	}
}

func TestDiscoverRootIsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Discover(f)
	if c := codeOf(t, err); c.Code != errs.CodeInternal {
		t.Fatalf("a root that is a file must stay an internal error even on Windows readdir(file), got %v", err)
	}
}

func TestFingerprintBlobDirUnsetAndErrors(t *testing.T) {
	s := fingerprintSource(t)
	s.BlobDir = ""
	if _, err := FingerprintOf(s); err != nil {
		t.Fatalf("no blob dir configured: %v", err)
	}
	s = fingerprintSource(t)
	old := walkDir
	t.Cleanup(func() { walkDir = old })
	for name, c := range map[string]struct {
		err  error
		code string
	}{
		"permission": {&fs.PathError{Op: "open", Path: s.LevelDBDir, Err: fs.ErrPermission}, errs.CodeNoFullDiskAccess},
		"other":      {errors.New("disk on fire"), ""},
	} {
		walkDir = func(root string, fn fs.WalkDirFunc) error { return fn(root, nil, c.err) }
		_, err := FingerprintOf(s)
		var coded *errs.Coded
		if !errors.As(err, &coded) {
			t.Fatalf("%s: err = %v, want a coded error", name, err)
		}
		if c.code != "" && coded.Code != c.code {
			t.Errorf("%s: code = %s, want %s", name, coded.Code, c.code)
		}
		if c.code == "" && !strings.Contains(err.Error(), "fingerprint leveldb") {
			t.Errorf("%s: err = %v, want the directory named", name, err)
		}
	}
}

func TestFingerprintStatFailureAndSubdirectories(t *testing.T) {
	s := fingerprintSource(t)
	if err := os.MkdirAll(filepath.Join(s.LevelDBDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := FingerprintOf(s); err != nil {
		t.Fatalf("a subdirectory must not fail the walk: %v", err)
	}
	old := statEntry
	statEntry = func(fs.DirEntry) (fs.FileInfo, error) { return nil, errors.New("stat failed") }
	t.Cleanup(func() { statEntry = old })
	if _, err := FingerprintOf(s); err == nil || !strings.Contains(err.Error(), "stat failed") {
		t.Fatalf("err = %v, want the stat failure", err)
	}
}

func TestFingerprintRelError(t *testing.T) {
	s := fingerprintSource(t)
	old := walkDir
	t.Cleanup(func() { walkDir = old })
	// A path relative to the root cannot be made from an absolute path and a relative root.
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) == 0 {
			return err
		}
		return fn("relative/elsewhere", fs.FileInfoToDirEntry(mustInfo(t, filepath.Join(root, entries[0].Name()))), nil)
	}
	if _, err := FingerprintOf(s); err == nil {
		t.Fatal("an unrelatable path was accepted")
	}
}

func mustInfo(t *testing.T, p string) fs.FileInfo {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestAccessors(t *testing.T) {
	if got := idStr(float64(42)); got != "42" {
		t.Errorf("idStr(42.0) = %q", got)
	}
	if got := idStr(float64(1.5)); got != "" {
		t.Errorf("idStr(1.5) = %q, want empty", got)
	}
	if got := idStr(math.Inf(1)); got != "" {
		t.Errorf("idStr(+Inf) = %q, want empty", got)
	}
	if got := idStr(int64(-7)); got != "-7" {
		t.Errorf("idStr(int64) = %q", got)
	}
	if got := idStr(true); got != "" {
		t.Errorf("idStr(bool) = %q", got)
	}
	if got := asInt(int64(9)); got != 9 {
		t.Errorf("asInt(int64) = %d", got)
	}
	if got := asInt(math.NaN()); got != 0 {
		t.Errorf("asInt(NaN) = %d", got)
	}
	if got := asInt(1e30); got != 0 {
		t.Errorf("asInt(1e30) = %d, want 0 rather than an overflowed value", got)
	}
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := parseTime(ts); !got.Equal(ts) {
		t.Errorf("parseTime(time.Time) = %v", got)
	}
	if got := asTime("2024-01-02T03:04:05Z"); !got.Equal(ts) {
		t.Errorf("asTime(RFC3339) = %v", got)
	}
	if got := asTime("5"); !got.IsZero() {
		t.Errorf("asTime(small digits) = %v, want zero", got)
	}
	if got := asTime(time.Time{}); !got.IsZero() {
		t.Errorf("asTime(zero) = %v", got)
	}
	if got := asTime(true); !got.IsZero() {
		t.Errorf("asTime(bool) = %v", got)
	}
	if got := items(&v8.ArrayWithProps{Items: []any{"a"}}); len(got) != 1 || got[0] != "a" {
		t.Errorf("items(ArrayWithProps) = %v", got)
	}
	if got := items(`[1, "x", {"k": [true]}]`); len(got) != 3 {
		t.Errorf("items(JSON string) = %v", got)
	}
	if got := items("[not json"); got != nil {
		t.Errorf("items(bad JSON) = %v", got)
	}
	if got := items(float64(1)); got != nil {
		t.Errorf("items(number) = %v", got)
	}
	if o := object(`{"b": 1, "a": {"c": 2}}`); o == nil || strings.Join(o.Keys, ",") != "a,b" {
		t.Errorf("object(JSON string) = %+v", o)
	}
	if o := object("{broken"); o != nil {
		t.Errorf("object(bad JSON) = %+v", o)
	}
	if o := object(float64(1)); o != nil {
		t.Errorf("object(number) = %+v", o)
	}
	if got := firstTime(); !got.IsZero() {
		t.Errorf("firstTime() = %v", got)
	}
	if got := firstTime(time.Time{}, ts); !got.Equal(ts) {
		t.Errorf("firstTime = %v", got)
	}
}

func TestIsPinnedShapes(t *testing.T) {
	cases := map[string]struct {
		v    any
		want bool
	}{
		"bool true":             {true, true},
		"bool false":            {false, false},
		"nil":                   {nil, false},
		"object with creator":   {obj("creatorId", "8:orgid:x"), true},
		"object with time":      {obj("pinnedTime", float64(5)), true},
		"empty object":          {obj(), false},
		"JSON string with time": {`{"pinnedTime": 7}`, true},
		"number":                {float64(1), false},
	}
	for name, c := range cases {
		if got := isPinned(c.v); got != c.want {
			t.Errorf("%s: isPinned = %v, want %v", name, got, c.want)
		}
	}
}

func TestMappersSkipEmptyEntries(t *testing.T) {
	if got := mapMentions([]any{obj(), obj("mri", "8:orgid:a", "displayName", "A"), obj("displayName", "Only Name")}); len(got) != 2 || got[0].ID != "8:orgid:a" || got[1].DisplayName != "Only Name" {
		t.Errorf("mapMentions = %+v", got)
	}
	props := obj("emotions", []any{obj(), obj("key", "like", "users", []any{"8:orgid:a", obj("mri", "8:orgid:b"), obj()})})
	got := mapReactions(props)
	if len(got) != 1 || got[0].Key != "like" || got[0].Count != 3 || len(got[0].UserIDs) != 2 {
		t.Errorf("mapReactions = %+v", got)
	}
	delta := obj("deltaEmotions", []any{obj("key", "heart")})
	if got := mapReactions(delta); len(got) != 1 || got[0].Key != "heart" {
		t.Errorf("deltaEmotions = %+v", got)
	}
	if got := mapFiles([]any{obj(), obj("fileName", "a.txt")}); len(got) != 1 || got[0].Name != "a.txt" {
		t.Errorf("mapFiles = %+v", got)
	}
	if got := mapLinks([]any{"", obj(), "https://x", obj("url", "https://y")}); len(got) != 2 {
		t.Errorf("mapLinks = %v", got)
	}
}

// A record holding a value the canonical encoder cannot render is unmapped, not a crash.
func TestUncanonicalizableRecordsAreUnmapped(t *testing.T) {
	bad := struct{}{}
	var um *UnmappedError
	msg := obj("conversationId", "19:x", "replyChainId", "1", "messageMap", obj("1", obj("id", "1", "weird", bad)))
	if _, _, err := MapReplyChain(acct1, msg); !errors.As(err, &um) || !strings.Contains(um.Reason, "not_canonicalizable") {
		t.Errorf("MapReplyChain err = %v", err)
	}
	if _, _, err := MapConversation(acct1, obj("id", "19:x", "weird", bad)); !errors.As(err, &um) || !strings.Contains(um.Reason, "not_canonicalizable") {
		t.Errorf("MapConversation err = %v", err)
	}
	if _, err := MapActivity(acct1, obj("activityId", "a", "weird", bad)); !errors.As(err, &um) || !strings.Contains(um.Reason, "not_canonicalizable") {
		t.Errorf("MapActivity err = %v", err)
	}
}

func TestConversationMemberWithoutIDIsSkipped(t *testing.T) {
	c, _, err := MapConversation(acct1, obj("id", "19:x", "members", []any{obj(), obj("id", "8:orgid:a", "friendlyName", "A")}))
	if err != nil || len(c.Members) != 1 {
		t.Fatalf("members = %v, %v", c.Members, err)
	}
}

func TestHTMLToTextEdges(t *testing.T) {
	cases := map[string]string{
		"unterminated script":   "a<script>never closed",
		"unterminated comment":  "a<!-- never closed",
		"unterminated tag":      "a<b never closed",
		"lone less-than at end": "a <",
		"less-than then space":  "1 < 2",
		"script then text":      "<script>x</script>after",
		"emoji alt":             `<emoji alt="smile"></emoji> hi`,
		"emoji image":           `<img itemtype="http://schema.skype.com/Emoji" alt="wink"> hi`,
		"plain image":           `<img src="x.png" alt="photo"> hi`,
	}
	want := map[string]string{
		"unterminated script":   "a",
		"unterminated comment":  "a",
		"unterminated tag":      "a",
		"lone less-than at end": "a <",
		"less-than then space":  "1 < 2",
		"script then text":      "after",
		"emoji alt":             "smile hi",
		"emoji image":           "wink hi",
		"plain image":           "hi",
	}
	for name, in := range cases {
		if got := HTMLToText(in); got != want[name] {
			t.Errorf("%s: HTMLToText(%q) = %q, want %q", name, in, got, want[name])
		}
	}
}

func TestAttrValueForms(t *testing.T) {
	cases := []struct {
		attrs, key, want string
	}{
		{` alt="a &amp; b"`, "alt", "a & b"},
		{` alt='single'`, "alt", "single"},
		{` alt=bare other=x`, "alt", "bare"},
		{` alt=bare`, "alt", "bare"},
		{` alt="unterminated`, "alt", ""},
		{` alt=`, "alt", ""},
		{` data-alt="no" alt="yes"`, "alt", "yes"},
		{` data-alt="no"`, "alt", ""},
		{` ALT="Upper"`, "alt", "Upper"},
		{``, "alt", ""},
	}
	for _, c := range cases {
		if got := attrValue(c.attrs, c.key); got != c.want {
			t.Errorf("attrValue(%q, %q) = %q, want %q", c.attrs, c.key, got, c.want)
		}
	}
}

func TestTagStartAtEnd(t *testing.T) {
	if tagStart("a<", 1) {
		t.Fatal("a '<' at the very end is not a tag")
	}
	if !tagStart("<a>", 0) || !tagStart("</a>", 0) || tagStart("< a>", 0) {
		t.Fatal("tagStart misjudged")
	}
}

func TestReadOpenFailureIsClassified(t *testing.T) {
	_, err := Read(context.Background(), t.TempDir(), nil, func(Account, string, any) error { return nil })
	if c := codeOf(t, err); c.Code != errs.CodeSnapshotInconsistent {
		t.Fatalf("an empty snapshot dir: %v", err)
	}
}

type dbsErrOrigin struct {
	fakeOrigin
	err error
}

func (d *dbsErrOrigin) Databases() ([]indexeddb.Database, error) { return nil, d.err }

func TestReadDatabasesFailureIsClassified(t *testing.T) {
	o := &dbsErrOrigin{err: fmt.Errorf("scan: %w", leveldb.ErrManifestTruncated)}
	_, err := readOrigin(context.Background(), o, nil, func(Account, string, any) error { return nil })
	if c := codeOf(t, err); c.Code != errs.CodeSnapshotInconsistent {
		t.Fatalf("err = %v", err)
	}
}

func TestReadSkipsForeignAndOtherAccountDatabases(t *testing.T) {
	stores := []indexeddb.Store{{ID: 1, Name: "conversations"}}
	f := &fakeOrigin{dbs: []indexeddb.Database{
		{ID: 1, Name: "not-a-teams-db", Stores: stores},
		{ID: 2, Name: dbName("conversation-manager", tenant2, user2), Stores: stores},
	}}
	f.decode = func(int64, []byte) (any, error) { t.Fatal("decoded a record that must be skipped"); return nil, nil }
	f.records = map[int64][]indexeddb.Record{2: {{Raw: []byte{1}}}}
	acct := Account{TenantID: tenant1, UserID: user1}
	if _, err := readOrigin(context.Background(), f, &acct, func(Account, string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestReadNonOmissionErrorsAreCoded(t *testing.T) {
	db := indexeddb.Database{ID: 1, Name: dbName("conversation-manager", tenant1, user1), Stores: []indexeddb.Store{{ID: 1, Name: "conversations"}}}
	boom := errors.New("decode machinery failed")
	// A decode failure that is not an omission is fatal and coded as a database error.
	f := &fakeOrigin{dbs: []indexeddb.Database{db}, records: map[int64][]indexeddb.Record{1: {{Raw: []byte{1}}}}}
	f.decode = func(int64, []byte) (any, error) { return nil, boom }
	_, err := readOrigin(context.Background(), f, nil, func(Account, string, any) error { return nil })
	if c := codeOf(t, err); c.Code != errs.CodeDBError {
		t.Fatalf("err = %v", err)
	}
	// A record whose key failed to decode with a non-omission error is also fatal.
	f = &fakeOrigin{dbs: []indexeddb.Database{db}, records: map[int64][]indexeddb.Record{1: {{Err: boom}}}}
	if _, err := readOrigin(context.Background(), f, nil, func(Account, string, any) error { return nil }); err == nil {
		t.Fatal("a non-omission record error was swallowed")
	}
}

func TestCallbackErrorUnwrapsAndPrints(t *testing.T) {
	inner := errors.New("caller says no")
	cb := &callbackError{inner}
	if cb.Error() != inner.Error() || !errors.Is(cb, inner) {
		t.Fatalf("callbackError = %v", cb)
	}
	if got := classifyRead(cb); !errors.Is(got, inner) {
		t.Fatalf("classifyRead(callbackError) = %v, want the caller's own error", got)
	}
	if got := classifyRead(context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("classifyRead(deadline) = %v", got)
	}
}

func TestFingerprintIgnoresSymlinks(t *testing.T) {
	s := fingerprintSource(t)
	want, err := FingerprintOf(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.LevelDBDir, "CURRENT"), filepath.Join(s.LevelDBDir, "link")); err != nil {
		t.Fatal(err)
	}
	if got, err := FingerprintOf(s); err != nil || got != want {
		t.Fatalf("a symlink changed the fingerprint: %v", err)
	}
}
