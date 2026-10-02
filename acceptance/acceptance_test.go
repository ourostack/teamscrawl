//go:build acceptance

// Package acceptance checks teamscrawl against the real Teams cache on this Mac. The tests run
// only with TEAMSCRAWL_REAL_CACHE=1 and Full Disk Access (make acceptance). The cache is private
// data: every log line and failure message here carries counts, timings, field paths and short
// hashes only, never a name, id or content.
//
// References: acceptance/diff.mjs (Node's own V8 deserializer, for the decoder) and ccl_count.py
// (ccl_chromium_reader, for the record and message counts). Both need local tools: node, python3
// and the reference clones described in ccl_count.py.
package acceptance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ourostack/teamscrawl/internal/indexeddb"
	"github.com/ourostack/teamscrawl/internal/leveldb"
	"github.com/ourostack/teamscrawl/internal/store"
	"github.com/ourostack/teamscrawl/internal/syncer"
	"github.com/ourostack/teamscrawl/internal/teamsdesktop"
	"github.com/ourostack/teamscrawl/internal/v8"
)

// allowlist mirrors the databases and stores teamsdesktop.Read is allowed to decode.
var allowlist = map[string]string{
	"replychain-manager":   "replychains-2",
	"conversation-manager": "conversations",
	"activity-manager":     "feed-items",
}

func keepAllowlisted(name string) bool {
	manager, _, ok := teamsdesktop.ParseDatabaseName(name)
	if !ok {
		return false
	}
	_, ok = allowlist[manager]
	return ok
}

// --- shared snapshot ---------------------------------------------------------

type snapshot struct {
	dir     string
	cleanup func()
}

var (
	snapOnce  sync.Once
	snaps     []snapshot
	snapErr   error
	snapTook  time.Duration
	syncOnce  sync.Once
	syncRep   syncer.Report
	syncErr   error
	syncTook  time.Duration
	archive   string
	archiveMu sync.Mutex
)

func TestMain(m *testing.M) {
	code := m.Run()
	for _, s := range snaps {
		s.cleanup()
	}
	// The archive directory is flat: remove its files, then the directory, never recursively.
	if archive != "" {
		ents, _ := os.ReadDir(archive)
		for _, e := range ents {
			_ = os.Remove(filepath.Join(archive, e.Name()))
		}
		_ = os.Remove(archive)
	}
	os.Exit(code)
}

func requireReal(t *testing.T) {
	t.Helper()
	if os.Getenv("TEAMSCRAWL_REAL_CACHE") != "1" {
		t.Skip("set TEAMSCRAWL_REAL_CACHE=1 to run against the real Teams cache")
	}
}

// snapshots copies every Teams origin of the live cache once and shares the copies between tests.
func snapshots(t *testing.T) []snapshot {
	t.Helper()
	requireReal(t)
	snapOnce.Do(func() {
		start := time.Now()
		srcs, _, err := teamsdesktop.Discover(teamsdesktop.DefaultRoot())
		if err != nil {
			snapErr = err
			return
		}
		for _, s := range srcs {
			dir, cleanup, err := teamsdesktop.Snapshot(context.Background(), s)
			if err != nil {
				snapErr = err
				return
			}
			snaps = append(snaps, snapshot{dir, cleanup})
		}
		snapTook = time.Since(start)
	})
	if snapErr != nil {
		t.Fatalf("snapshot: %v", snapErr)
	}
	return snaps
}

// synced runs one full sync of the live cache into a scratch archive and shares the result.
func synced(t *testing.T) (syncer.Report, string) {
	t.Helper()
	requireReal(t)
	syncOnce.Do(func() {
		dir, err := os.MkdirTemp("", "teamscrawl-acceptance-")
		if err != nil {
			syncErr = err
			return
		}
		archiveMu.Lock()
		archive = dir
		archiveMu.Unlock()
		start := time.Now()
		syncRep, _, syncErr = syncer.Run(context.Background(), syncer.Options{DBPath: filepath.Join(dir, "archive.db")})
		syncTook = time.Since(start)
	})
	if syncErr != nil {
		t.Fatalf("sync: %v", syncErr)
	}
	return syncRep, filepath.Join(archive, "archive.db")
}

func h12(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

func openSnapshot(t *testing.T, dir string) *indexeddb.Origin {
	t.Helper()
	o, err := indexeddb.OpenWith(filepath.Join(dir, "leveldb"), filepath.Join(dir, "blob"), indexeddb.OpenOptions{KeepDatabase: keepAllowlisted})
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	return o
}

// forEachRecord calls fn for every record of every allowlisted store.
func forEachRecord(o *indexeddb.Origin, fn func(manager string, dbID int64, r indexeddb.Record)) error {
	dbs, err := o.Databases()
	if err != nil {
		return err
	}
	for _, db := range dbs {
		manager, _, ok := teamsdesktop.ParseDatabaseName(db.Name)
		if !ok || allowlist[manager] == "" {
			continue
		}
		for _, s := range db.Stores {
			if s.Name != allowlist[manager] {
				continue
			}
			if err := o.Records(db.ID, s.ID, func(r indexeddb.Record) error {
				fn(manager, db.ID, r)
				return nil
			}); err != nil {
				return fmt.Errorf("records of %s: %w", manager, err)
			}
		}
	}
	return nil
}

// --- TestRealDifferential ----------------------------------------------------

type goSide struct {
	canon  string // canonical JSON when decoding succeeded
	failed string // omission-style reason when Deserialize failed
}

func TestRealDifferential(t *testing.T) {
	snapshots := snapshots(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node is required: %v", err)
	}
	var total, checked, notCheckable, noPayload, mismatches int
	mismatchPaths := map[string]int{}
	notCheckableWhy := map[string]int{}
	start := time.Now()
	for _, snap := range snapshots {
		o := openSnapshot(t, snap.dir)
		cmd := exec.Command(node, "diff.mjs") //nolint:gosec // node comes from PATH; the script is this package's own
		cmd.Dir = diffScriptDir(t)
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		results := make(chan goSide, 256)
		writeErr := make(chan error, 1)
		go func() {
			defer close(results)
			var err error
			func() {
				defer func() { _ = stdin.Close() }()
				if ferr := forEachRecord(o, func(manager string, dbID int64, r indexeddb.Record) {
					if err != nil {
						return
					}
					if r.Err != nil {
						noPayload++
						return
					}
					payload, perr := o.Payload(dbID, r.Raw)
					if perr != nil {
						noPayload++
						return
					}
					var side goSide
					v, derr := v8.Deserialize(payload)
					if derr != nil {
						side.failed = "go_decode_failed"
					} else if c, cerr := v8.Canonical(v); cerr != nil {
						side.failed = "go_canonical_failed"
					} else {
						side.canon = string(c)
					}
					results <- side
					// One unbuffered write per record: the reader waits for Node's answer to each
					// record, so nothing may sit in a buffer.
					frame := make([]byte, 4, 4+len(payload))
					binary.BigEndian.PutUint32(frame, uint32(len(payload))) //nolint:gosec // payloads are far below 4 GiB
					_, err = stdin.Write(append(frame, payload...))
				}); ferr != nil && err == nil {
					err = ferr
				}
			}()
			writeErr <- err
		}()
		rd := bufio.NewReaderSize(stdout, 1<<20)
		for side := range results {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("reading node output after %d records: %v; %s", total, err, stderrNote(t, "node", stderr.Bytes()))
			}
			line = strings.TrimSuffix(line, "\n")
			total++
			kind, rest, _ := strings.Cut(line, " ")
			switch {
			case kind == "nc":
				notCheckable++
				notCheckableWhy[rest]++
			case side.failed != "":
				checked++
				mismatches++
				mismatchPaths["<go: "+side.failed+", node decoded it>"]++
			default:
				checked++
				if rest != side.canon {
					mismatches++
					mismatchPaths[firstDifference(side.canon, rest)]++
				}
			}
		}
		if _, err := rd.ReadByte(); !errors.Is(err, io.EOF) {
			t.Errorf("node printed more lines than records sent")
		}
		if err := <-writeErr; err != nil {
			t.Fatalf("writing to node: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("node: %v; %s", err, stderrNote(t, "node", stderr.Bytes()))
		}
	}
	t.Logf("differential: records=%d checked=%d not_checkable=%d no_payload=%d mismatches=%d in %v (snapshot %v)",
		total, checked, notCheckable, noPayload, mismatches, time.Since(start).Round(time.Millisecond), snapTook.Round(time.Millisecond))
	for _, k := range sortedKeys(notCheckableWhy) {
		t.Logf("  not_checkable %q: %d", k, notCheckableWhy[k])
	}
	for _, k := range sortedKeys(mismatchPaths) {
		t.Logf("  mismatch at %s: %d", k, mismatchPaths[k])
	}
	if total == 0 {
		t.Fatal("no allowlisted records were compared")
	}
	allowlisted := total + noPayload
	if noPayload != 0 {
		t.Errorf("%d of %d allowlisted records yielded no payload to compare", noPayload, allowlisted)
	}
	if notCheckable*1000 > allowlisted*5 {
		t.Errorf("%d of %d allowlisted records are not checkable by Node, more than 0.5%%", notCheckable, allowlisted)
	}
	if checked*100 < allowlisted*99 {
		t.Errorf("only %d of %d allowlisted records were checked, want at least 99%%", checked, allowlisted)
	}
	if mismatches > 0 {
		t.Errorf("%d of %d checked records decode differently from Node (paths above)", mismatches, checked)
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- reference counts --------------------------------------------------------

type refCounts struct {
	Reference     string `json:"reference"`
	Conversations struct {
		RecordVersions   int `json:"record_versions"`
		DistinctKeys     int `json:"distinct_keys"`
		LatestLive       int `json:"latest_live"`
		LatestTombstoned int `json:"latest_tombstoned"`
	} `json:"conversations"`
	Messages struct {
		LatestDistinct int `json:"latest_distinct"`
		Chains         int `json:"chains"`
	} `json:"messages"`
	Undecodable struct {
		Conversations []string `json:"conversations"`
		Chains        []string `json:"chains"`
	} `json:"undecodable"`
	Hashes struct {
		Conversations []string `json:"conversations"`
		Messages      []string `json:"messages"`
	} `json:"hashes"`
}

// requireReference fails with setup instructions when a reference clone is missing. It is a
// failure, not a skip: the acceptance run is not complete without the reference counts.
func requireReference(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	for _, r := range []struct{ env, def, module, url string }{
		{"CCL_READER", "~/code/_refs/ccl_chromium_reader", "ccl_chromium_reader", "https://github.com/cclgroupltd/ccl_chromium_reader"},
		{"CCL_SNAPPY", "~/code/_refs/ccl_simplesnappy", "ccl_simplesnappy", "https://github.com/cclgroupltd/ccl_simplesnappy"},
	} {
		dir := os.Getenv(r.env)
		if dir == "" {
			dir = r.def
		}
		dir = strings.Replace(dir, "~", home, 1)
		if _, err := os.Stat(dir); err != nil { //nolint:gosec // developer-chosen reference path
			t.Fatalf("reference %s not found at %s. Clone %s there (default %s) or point %s at an existing clone; python3 needs no pip packages.",
				r.module, dir, r.url, r.def, r.env)
		}
	}
}

func runCCL(t *testing.T, snapDir string) refCounts {
	t.Helper()
	py := os.Getenv("TEAMSCRAWL_PYTHON")
	if py == "" {
		py = "python3"
	}
	requireReference(t)
	start := time.Now()
	cmd := exec.Command(py, "ccl_count.py", snapDir) //nolint:gosec // the interpreter is the developer's own choice
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ccl_count.py: %v; %s", err, stderrNote(t, "ccl_count", stderr.Bytes()))
	}
	var rc refCounts
	if err := json.Unmarshal(out, &rc); err != nil {
		t.Fatalf("ccl_count.py output: %v", err)
	}
	t.Logf("reference %s counted in %v", rc.Reference, time.Since(start).Round(time.Millisecond))
	return rc
}

// ours holds what teamscrawl's own reader and mapper produce from one snapshot.
type ours struct {
	convKeys      map[string]bool     // hashes of the record keys of the conversations store
	convIDs       map[string]bool     // hashes of the mapped conversation ids
	messages      map[string]bool     // hashes of conversation id + message id
	chainMsgs     map[string][]string // record key hash (parts joined with NUL) -> its message hashes
	rawVersions   int                 // every stored version of a conversation record, from the LevelDB reader
	omissions     map[string]int      // Read's omission counts
	keyMismatches int                 // conversation records mapped under an id other than their key
}

func readOurs(t *testing.T, snapDir string) ours {
	t.Helper()
	o := ours{convKeys: map[string]bool{}, convIDs: map[string]bool{}, messages: map[string]bool{}, chainMsgs: map[string][]string{}}
	// Conversation record keys, straight from the store (not through the mapper).
	idb := openSnapshot(t, snapDir)
	dbs, err := idb.Databases()
	if err != nil {
		t.Fatal(err)
	}
	type target struct{ db, store int64 }
	var convStores []target
	for _, db := range dbs {
		manager, _, ok := teamsdesktop.ParseDatabaseName(db.Name)
		if ok && manager == "conversation-manager" {
			for _, s := range db.Stores {
				if s.Name == "conversations" {
					convStores = append(convStores, target{db.ID, s.ID})
				}
			}
		}
	}
	for _, c := range convStores {
		_ = idb.Records(c.db, c.store, func(r indexeddb.Record) error {
			if k, ok := r.Key.(string); ok && r.Err == nil {
				o.convKeys[h12(k)] = true
			}
			return nil
		})
		// Every version, tombstones included: Keep sees each one before newest-wins.
		if c.db >= 256 || c.store >= 256 {
			t.Fatalf("database or store id too large for the raw version count: %d, %d", c.db, c.store)
		}
		{
			prefix := []byte{0, byte(c.db), byte(c.store), 1} //nolint:gosec // both ids are below 256 here
			if _, err := leveldb.LoadWith(filepath.Join(snapDir, "leveldb"), leveldb.LoadOptions{Keep: func(k []byte) bool {
				if bytes.HasPrefix(k, prefix) {
					o.rawVersions++
				}
				return false
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Per-record attribution of messages, to explain differences with the reference.
	for _, db := range dbs {
		manager, acct, ok := teamsdesktop.ParseDatabaseName(db.Name)
		if !ok || manager != "replychain-manager" {
			continue
		}
		for _, s := range db.Stores {
			if s.Name != "replychains-2" {
				continue
			}
			_ = idb.Records(db.ID, s.ID, func(r indexeddb.Record) error {
				parts, _ := r.Key.([]any)
				var texts []string
				for _, p := range parts {
					texts = append(texts, fmt.Sprint(p))
				}
				v, err := idb.Decode(db.ID, r.Raw)
				if err != nil || r.Err != nil {
					return nil
				}
				ms, _, err := teamsdesktop.MapReplyChain(acct, v)
				if err != nil {
					return nil
				}
				k := h12(strings.Join(texts, "\x00"))
				for _, m := range ms {
					o.chainMsgs[k] = append(o.chainMsgs[k], h12(m.ConversationID+"\x00"+m.ID))
				}
				return nil
			})
		}
	}
	om, err := teamsdesktop.Read(context.Background(), snapDir, nil, func(a teamsdesktop.Account, kind string, v any) error {
		switch kind {
		case teamsdesktop.KindConversation:
			c, _, err := teamsdesktop.MapConversation(a, v)
			if err != nil {
				return err
			}
			o.convIDs[h12(c.ID)] = true
		case teamsdesktop.KindReplyChain:
			ms, _, err := teamsdesktop.MapReplyChain(a, v)
			if err != nil {
				return err
			}
			for _, m := range ms {
				o.messages[h12(m.ConversationID+"\x00"+m.ID)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	o.omissions = om
	for k := range o.convKeys {
		if !o.convIDs[k] {
			o.keyMismatches++
		}
	}
	return o
}

func missing(want []string, have map[string]bool) int {
	n := 0
	for _, h := range want {
		if !have[h] {
			n++
		}
	}
	return n
}

// TestRealConversationAccounting accounts for every stored version of a conversation record:
// each is superseded by a newer version of its key, a tombstone, undecodable, unmapped, mapped
// under another id, or mapped. Nothing may be left over, and the reference decoder must see
// exactly the keys teamscrawl maps.
func TestRealConversationAccounting(t *testing.T) {
	for _, snap := range snapshots(t) {
		rc := runCCL(t, snap.dir)
		us := readOurs(t, snap.dir)
		superseded := rc.Conversations.RecordVersions - rc.Conversations.DistinctKeys
		undecodable := 0
		for code, n := range us.omissions {
			if code != "truncated_log_tail" {
				undecodable += n
			}
		}
		t.Logf("conversation record versions: reference=%d raw_leveldb=%d", rc.Conversations.RecordVersions, us.rawVersions)
		t.Logf("  superseded by a newer version: %d", superseded)
		t.Logf("  tombstoned (newest version is a deletion): %d", rc.Conversations.LatestTombstoned)
		t.Logf("  omissions of every kind (conversations are the only kind that could drop a conversation): %d %v", undecodable, us.omissions)
		t.Logf("  mapped under a different id than the record key: %d", us.keyMismatches)
		t.Logf("  mapped as conversations: %d (reference newest live: %d)", len(us.convIDs), rc.Conversations.LatestLive)
		droppedFromRef := missing(rc.Hashes.Conversations, us.convIDs)
		t.Logf("  reference conversations teamscrawl does not map (genuinely dropped): %d", droppedFromRef)
		if us.rawVersions != rc.Conversations.RecordVersions {
			t.Errorf("LevelDB reader sees %d conversation record versions, reference %d", us.rawVersions, rc.Conversations.RecordVersions)
		}
		if droppedFromRef != 0 {
			t.Errorf("%d reference conversations are not mapped", droppedFromRef)
		}
		if extra := len(us.convIDs) - (rc.Conversations.LatestLive - droppedFromRef); extra != 0 {
			t.Errorf("teamscrawl maps %d conversations the reference does not have", extra)
		}
		accounted := superseded + rc.Conversations.LatestTombstoned + undecodable + us.keyMismatches + len(us.convIDs)
		if accounted != rc.Conversations.RecordVersions {
			t.Errorf("%d of %d conversation record versions are unaccounted for", rc.Conversations.RecordVersions-accounted, rc.Conversations.RecordVersions)
		}
	}
}

// TestRealVolumes compares teamscrawl's counts with the reference decoder's on the same
// snapshot. The reference counts only the newest version of each record and drops tombstones
// (superseded versions and deletions are correct to omit); teamscrawl must map at least 99% of
// that. It then checks the archive built from the live cache holds at least 99% too.
func TestRealVolumes(t *testing.T) {
	var refConvs, refMsgs, ourConvs, ourMsgs int
	for _, snap := range snapshots(t) {
		rc := runCCL(t, snap.dir)
		us := readOurs(t, snap.dir)
		refConvs += rc.Conversations.LatestLive
		refMsgs += rc.Messages.LatestDistinct
		ourConvs += len(us.convIDs) - missing(keys(us.convIDs), toSet(rc.Hashes.Conversations))
		ourMsgs += messageAccounting(t, rc, us)
	}
	t.Logf("reference (newest live versions): conversations=%d messages=%d; teamscrawl matching: conversations=%d messages=%d", refConvs, refMsgs, ourConvs, ourMsgs)
	atLeast99(t, "conversations", ourConvs, refConvs)
	atLeast99(t, "messages", ourMsgs, refMsgs)

	rep, db := synced(t)
	st, err := store.OpenReadOnly(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	archConvs, archMsgs := count(t, st, "select count(*) from conversations"), count(t, st, "select count(*) from messages")
	t.Logf("archive after sync (%v, status %s): conversations=%d messages=%d omissions=%v", syncTook.Round(time.Millisecond), rep.Status, archConvs, archMsgs, rep.Omissions)
	atLeast99(t, "archive conversations", archConvs, refConvs)
	atLeast99(t, "archive messages", archMsgs, refMsgs)
}

// messageAccounting compares teamscrawl's messages with the reference's. Reference messages that
// teamscrawl lacks are defects. Messages only teamscrawl has are fine when they sit in a record
// the reference could not decode (ccl_chromium_reader fails on a few large blob-backed values
// that teamscrawl reads); any other difference is a defect.
func messageAccounting(t *testing.T, rc refCounts, us ours) (matching int) {
	t.Helper()
	ref := toSet(rc.Hashes.Messages)
	refFailed := toSet(rc.Undecodable.Chains)
	lacking := missing(rc.Hashes.Messages, us.messages)
	extra, extraInRefFailed := 0, 0
	seen := map[string]bool{}
	chainKeys := make([]string, 0, len(us.chainMsgs))
	for key := range us.chainMsgs {
		chainKeys = append(chainKeys, key)
	}
	sort.Strings(chainKeys) // a message under two record keys is attributed to the smallest key
	for _, key := range chainKeys {
		for _, m := range us.chainMsgs[key] {
			if ref[m] || seen[m] {
				continue
			}
			seen[m] = true
			extra++
			if refFailed[key] {
				extraInRefFailed++
			}
		}
	}
	t.Logf("messages: reference %d, teamscrawl %d; reference messages teamscrawl lacks: %d; teamscrawl-only: %d, of which in %d records the reference could not decode: %d",
		len(ref), len(us.messages), lacking, extra, len(refFailed), extraInRefFailed)
	if lacking != 0 {
		t.Errorf("teamscrawl lacks %d messages the reference has", lacking)
	}
	if extra != extraInRefFailed {
		t.Errorf("%d teamscrawl-only messages are not explained by records the reference could not decode", extra-extraInRefFailed)
	}
	return len(ref) - lacking
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func toSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, k := range s {
		m[k] = true
	}
	return m
}

func atLeast99(t *testing.T, what string, got, ref int) {
	t.Helper()
	if ref == 0 {
		t.Errorf("%s: reference count is 0", what)
		return
	}
	pct := 100 * float64(got) / float64(ref)
	t.Logf("%s: %d of %d (%.2f%%)", what, got, ref, pct)
	if pct < 99 {
		t.Errorf("%s: %d is %.2f%% of the reference %d, want at least 99%%", what, got, pct, ref)
	}
}

// acceptanceRowLimit is higher than any table here: these checks read whole results.
const acceptanceRowLimit = 1 << 30

func count(t *testing.T, st *store.Store, q string) int {
	t.Helper()
	_, rows, _, err := st.SQL(context.Background(), q, acceptanceRowLimit)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: %v", q, err)
	}
	switch n := rows[0][0].(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	t.Fatalf("%s: unexpected type %T", q, rows[0][0])
	return 0
}

// --- recency, blobs, auth ----------------------------------------------------

func scalarText(t *testing.T, st *store.Store, q string) time.Time {
	t.Helper()
	_, rows, _, err := st.SQL(context.Background(), q, acceptanceRowLimit)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%s: %v", q, err)
	}
	s, _ := rows[0][0].(string)
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%s: unparsable timestamp (%d chars): %v", q, len(s), err)
	}
	return ts
}

func TestRealRecency(t *testing.T) {
	_, db := synced(t)
	st, err := store.OpenReadOnly(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	newestMsg := scalarText(t, st, "select max(sent_at) from messages")
	newestConv := scalarText(t, st, "select max(last_message_at) from conversations")
	gap := newestConv.Sub(newestMsg)
	t.Logf("newest conversation activity is %v after the newest message sent_at (archive age %v)", gap.Round(time.Second), time.Since(newestMsg).Round(time.Minute))
	if gap > time.Hour || gap < -time.Hour {
		t.Errorf("newest sent_at is %v from the newest lastMessageTimeUtc, want within 1h", gap)
	}
}

func TestRealBlobsResolve(t *testing.T) {
	rep, _ := synced(t)
	t.Logf("sync status %s, omissions %v", rep.Status, rep.Omissions)
	if n := rep.Omissions["blob_missing"]; n != 0 {
		t.Errorf("blob_missing = %d, want 0", n)
	}
}

// jwt matches the shape of a bearer token: three base64url segments, the first two starting
// with "eyJ" (a base64 JSON header and payload).
var jwt = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)

// TestRealNoAuthDecoded checks that the Teams auth databases are present in the cache yet never
// read: every database outside the allowlist yields no records, only allowlisted record kinds are
// delivered, and the archive holds nothing shaped like a bearer token.
func TestRealNoAuthDecoded(t *testing.T) {
	for _, snap := range snapshots(t) {
		o := openSnapshot(t, snap.dir)
		dbs, err := o.Databases()
		if err != nil {
			t.Fatal(err)
		}
		var allowed, other, auth, otherRecords int
		for _, db := range dbs {
			if keepAllowlisted(db.Name) {
				allowed++
				continue
			}
			other++
			if strings.HasPrefix(db.Name, "Teams:auth:") {
				auth++
			}
			for _, s := range db.Stores {
				_ = o.Records(db.ID, s.ID, func(indexeddb.Record) error { otherRecords++; return nil })
			}
		}
		t.Logf("databases: allowlisted=%d other=%d (auth=%d); records readable from non-allowlisted databases=%d", allowed, other, auth, otherRecords)
		if otherRecords != 0 {
			t.Errorf("%d records were readable from non-allowlisted databases", otherRecords)
		}
		kinds := map[string]int{}
		if _, err := teamsdesktop.Read(context.Background(), snap.dir, nil, func(a teamsdesktop.Account, kind string, v any) error {
			kinds[kind]++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for kind := range kinds {
			switch kind {
			case teamsdesktop.KindReplyChain, teamsdesktop.KindConversation, teamsdesktop.KindActivity:
			default:
				t.Errorf("Read delivered records of kind %q", kind)
			}
		}
	}
	_, db := synced(t)
	st, err := store.OpenReadOnly(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// Message content may legitimately carry a token inside a shared link (a message the user
	// received said so), so token-shaped strings are tolerated in the message content columns
	// and nowhere else: not in conversations, people, activity or sync bookkeeping.
	contentColumns := map[string]bool{"messages.content_html": true, "messages.content_text": true, "messages.raw_json": true, "messages.links_json": true}
	_, tables, _, err := st.SQL(context.Background(), "select name from sqlite_master where type = 'table' and name not like '%fts%' and name not like 'sqlite_%'", acceptanceRowLimit)
	if err != nil {
		t.Fatal(err)
	}
	inContent, elsewhere := 0, 0
	for _, row := range tables {
		table, _ := row[0].(string)
		_, cols, _, err := st.SQL(context.Background(), fmt.Sprintf("select name from pragma_table_info('%s')", table), acceptanceRowLimit)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			col, _ := c[0].(string)
			_, vals, _, err := st.SQL(context.Background(), fmt.Sprintf("select cast(%q as text) from %q where cast(%q as text) like '%%eyJ%%'", col, table, col), acceptanceRowLimit)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, v := range vals {
				if s, ok := v[0].(string); ok {
					n += len(jwt.FindAllString(s, -1))
				}
			}
			if n == 0 {
				continue
			}
			t.Logf("token-shaped strings in %s.%s: %d", table, col, n)
			if contentColumns[table+"."+col] {
				inContent += n
			} else {
				elsewhere += n
			}
		}
	}
	t.Logf("token-shaped strings: %d in message content columns, %d elsewhere", inContent, elsewhere)
	if elsewhere != 0 {
		t.Errorf("%d token-shaped strings outside message content", elsewhere)
	}
}
