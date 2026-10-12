// The `pocketcasts` tool's worker. Its docs live in the sibling `.caos-expr`
// here-string; the README has the background.
//
// Pocket Casts has no public API. This talks to the same endpoints the
// open-source Android app (Automattic/pocket-casts-android) uses:
//
//	POST api.pocketcasts.com/user/login_pocket_casts   JSON    -> bearer token
//	POST api.pocketcasts.com/user/podcast/list         JSON/pb -> subscriptions, folders
//	POST api.pocketcasts.com/user/podcast/episodes     JSON    -> per-episode state (isDeleted == archived)
//	POST api.pocketcasts.com/user/sync/update          protobuf-> ALL writes: subscribe, folders, archive
//	GET  podcast-api.pocketcasts.com/podcast/full/UUID public  -> titles and episode lists
//
// The Go image has no module cache, so this is standard library only: the
// protobuf encoding is hand-written (it is a handful of fixed messages).
//
// Args under /cas/args: op, call, and the optional parameters read below.
// Secrets: /secret/pocketcasts-email and /secret/pocketcasts-password.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"caos/w"
)

const (
	apiHost    = "https://api.pocketcasts.com"
	metaHost   = "https://podcast-api.pocketcasts.com"
	homeFolder = "00000000-0000-0000-0000-000000000000" // "no folder"
	maxBody    = 100_000
	batchSize  = 100 // records per sync/update request
)

var out strings.Builder

func pf(format string, a ...any) { fmt.Fprintf(&out, format, a...) }

// ---- arguments -----------------------------------------------------------

func arg(name string) (string, bool) {
	path := "/cas/args/" + name
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	b, err := exec.Command("caos", "get", path).CombinedOutput()
	w.True(err == nil, "caos get %s: %v: %s", path, err, b)
	return string(w.Check(os.ReadFile(path))), true
}

func opt(name string) string {
	v, _ := arg(name)
	return strings.TrimSpace(v)
}

func required(name string) string {
	v := opt(name)
	w.True(v != "", "pocketcasts: %s is required", name)
	return v
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

func secret(name string) string {
	b, err := os.ReadFile("/secret/" + name)
	w.True(err == nil, "pocketcasts: the %s secret is not granted to this tool (README, \"Secrets\")", name)
	return strings.TrimRight(string(b), "\r\n")
}

// ---- HTTP ----------------------------------------------------------------

// The authenticated client follows no redirects, so the bearer token only ever
// goes to api.pocketcasts.com.
var apiClient = &http.Client{
	Timeout: 90 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// The public metadata endpoint redirects to a static file, and carries no
// credentials.
var pubClient = &http.Client{Timeout: 90 * time.Second}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func apiCall(path, ctype, token string, body []byte) (int, []byte) {
	req := w.Check(http.NewRequest("POST", apiHost+path, bytes.NewReader(body)))
	req.Header.Set("User-Agent", "caos-pocketcasts")
	req.Header.Set("Content-Type", ctype)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := apiClient.Do(req)
	w.True(err == nil, "pocketcasts: POST %s got no response: %v. A write may still have arrived: re-read the state before retrying.", path, err)
	defer resp.Body.Close()
	return resp.StatusCode, w.Check(io.ReadAll(io.LimitReader(resp.Body, 64<<20)))
}

func publicGet(u string) (int, []byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "caos-pocketcasts")
	resp, err := pubClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, data, err
}

func login() string {
	email, password := secret("pocketcasts-email"), secret("pocketcasts-password")
	body := w.Check(json.Marshal(map[string]string{"email": email, "password": password, "scope": "mobile"}))
	code, data := apiCall("/user/login_pocket_casts", "application/json", "", body)
	w.True(code == 200, "pocketcasts: login failed: HTTP %d: %s", code, snippet(data))
	var r struct {
		Token string `json:"token"`
	}
	w.Must(json.Unmarshal(data, &r))
	w.True(r.Token != "", "pocketcasts: login returned no token")
	return r.Token
}

// ---- protobuf ------------------------------------------------------------

func pbKey(num, wire int) []byte { return binary.AppendUvarint(nil, uint64(num<<3|wire)) }

func pbVarint(num int, v uint64) []byte {
	return append(pbKey(num, 0), binary.AppendUvarint(nil, v)...)
}

func pbBytes(num int, b []byte) []byte {
	o := binary.AppendUvarint(pbKey(num, 2), uint64(len(b)))
	return append(o, b...)
}

func pbStr(num int, s string) []byte { return pbBytes(num, []byte(s)) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// google.protobuf wrappers. A false BoolValue is an EMPTY message, still
// present: the field's presence is what says "set to false".
func boolW(num int, v bool) []byte {
	var inner []byte
	if v {
		inner = pbVarint(1, 1)
	}
	return pbBytes(num, inner)
}

func int64W(num int, v int64) []byte { return pbBytes(num, pbVarint(1, uint64(v))) }

func stringW(num int, s string) []byte {
	var inner []byte
	if s != "" {
		inner = pbStr(1, s)
	}
	return pbBytes(num, inner)
}

func timestamp(num int, t time.Time) []byte {
	return pbBytes(num, cat(pbVarint(1, uint64(t.Unix())), pbVarint(2, uint64(t.Nanosecond()))))
}

type pfield struct {
	num, wire int
	v         uint64
	b         []byte
}

func pbParse(b []byte) []pfield {
	var fields []pfield
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		w.True(n > 0, "pocketcasts: malformed protobuf response")
		b = b[n:]
		f := pfield{num: int(key >> 3), wire: int(key & 7)}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(b)
			w.True(n > 0, "pocketcasts: malformed protobuf varint")
			f.v, b = v, b[n:]
		case 1:
			w.True(len(b) >= 8, "pocketcasts: truncated protobuf")
			b = b[8:]
		case 5:
			w.True(len(b) >= 4, "pocketcasts: truncated protobuf")
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			w.True(n > 0 && uint64(len(b)-n) >= l, "pocketcasts: truncated protobuf")
			b = b[n:]
			f.b, b = b[:l], b[l:]
		default:
			w.True(false, "pocketcasts: unsupported protobuf wire type %d", f.wire)
		}
		fields = append(fields, f)
	}
	return fields
}

// ---- sync records (SyncUpdateRequest.records) ----------------------------

// Each builder returns a whole Record: the oneof field that names the kind.

// podcastRec is Record.podcast (1) = SyncUserPodcast.
func podcastRec(uuid string, subscribed *bool, folder *string, added *time.Time) []byte {
	m := pbStr(1, uuid)
	if subscribed != nil {
		m = cat(m, boolW(3, *subscribed))
	}
	if folder != nil {
		m = cat(m, stringW(7, *folder))
	}
	if added != nil {
		m = cat(m, timestamp(9, *added))
	}
	return pbBytes(1, m)
}

// episodeRec is Record.episode (2) = SyncUserEpisode. Pocket Casts calls an
// archived episode "deleted" on the wire (isDeleted); the _modified stamp is
// what the server's last-writer-wins compares.
func episodeRec(podcast, uuid string, archived bool, nowMs int64) []byte {
	return pbBytes(2, cat(pbStr(1, uuid), pbStr(2, podcast), boolW(3, archived), int64W(4, nowMs)))
}

// folderRec is Record.folder (5) = SyncUserFolder. Its scalars are plain
// proto3 fields, so an update must carry the folder's current values or it
// resets them.
func folderRec(f fold, deleted bool, added *time.Time) []byte {
	m := pbStr(1, f.UUID)
	if deleted {
		m = cat(m, pbVarint(2, 1))
	}
	if f.Name != "" {
		m = cat(m, pbStr(3, f.Name))
	}
	if f.Color != 0 {
		m = cat(m, pbVarint(4, uint64(f.Color)))
	}
	if f.SortPos != 0 {
		m = cat(m, pbVarint(5, uint64(f.SortPos)))
	}
	if f.SortType != 0 {
		m = cat(m, pbVarint(6, uint64(f.SortType)))
	}
	if added != nil {
		m = cat(m, timestamp(7, *added))
	}
	return pbBytes(5, m)
}

// syncUpdate sends records in batches. last_modified is "now", so the server
// has nothing to send back beyond the acknowledgement.
func syncUpdate(token string, records [][]byte) {
	for start := 0; start < len(records); start += batchSize {
		end := min(start+batchSize, len(records))
		now := uint64(time.Now().UnixMilli())
		req := cat(pbVarint(1, now), pbVarint(2, now), pbStr(4, "caos-pocketcasts"))
		for _, r := range records[start:end] {
			req = cat(req, pbBytes(5, r))
		}
		code, data := apiCall("/user/sync/update", "application/octet-stream", token, req)
		w.True(code == 200, "pocketcasts: sync/update failed for records %d-%d of %d: HTTP %d: %s. Earlier batches were applied; re-read the state before retrying.", start+1, end, len(records), code, snippet(data))
	}
}

func newUUID() string {
	var b [16]byte
	w.Check(rand.Read(b[:]))
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ---- the library: subscriptions and folders ------------------------------

type pod struct {
	UUID, Title, Author, Folder, Latest string
}

type fold struct {
	UUID, Name                string
	Color, SortPos, SortType int
}

type library struct {
	Pods    []pod
	Folders []fold
}

func gs(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

func gi(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok {
			return int(f)
		}
	}
	return 0
}

func noHome(u string) string {
	if u == homeFolder {
		return ""
	}
	return u
}

func parseLibraryJSON(data []byte) (library, bool) {
	var raw struct {
		Podcasts []map[string]any `json:"podcasts"`
		Folders  []map[string]any `json:"folders"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return library{}, false
	}
	var lib library
	for _, m := range raw.Podcasts {
		lib.Pods = append(lib.Pods, pod{
			UUID: gs(m, "uuid"), Title: gs(m, "title"), Author: gs(m, "author"),
			Folder: noHome(gs(m, "folderUuid", "folder_uuid")), Latest: gs(m, "lastEpisodePublished"),
		})
	}
	for _, m := range raw.Folders {
		lib.Folders = append(lib.Folders, fold{
			UUID: gs(m, "folderUuid", "uuid"), Name: gs(m, "name"), Color: gi(m, "color"),
			SortPos: gi(m, "sortPosition"), SortType: gi(m, "podcastsSortType"),
		})
	}
	return lib, true
}

// parseLibraryProto reads UserPodcastListResponse: podcasts (1), folders (2).
// The protobuf form carries no titles; fillTitles gets them.
func parseLibraryProto(data []byte) library {
	var lib library
	for _, f := range pbParse(data) {
		switch f.num {
		case 1:
			var p pod
			for _, g := range pbParse(f.b) {
				switch g.num {
				case 1:
					p.UUID = string(g.b)
				case 14: // folder_uuid: StringValue
					for _, h := range pbParse(g.b) {
						if h.num == 1 {
							p.Folder = noHome(string(h.b))
						}
					}
				}
			}
			lib.Pods = append(lib.Pods, p)
		case 2:
			var fo fold
			for _, g := range pbParse(f.b) {
				switch g.num {
				case 1:
					fo.UUID = string(g.b)
				case 2:
					fo.Name = string(g.b)
				case 3:
					fo.Color = int(g.v)
				case 4:
					fo.SortPos = int(g.v)
				case 5:
					fo.SortType = int(g.v)
				}
			}
			lib.Folders = append(lib.Folders, fo)
		}
	}
	return lib
}

func fetchLibrary(token string) library {
	code, data := apiCall("/user/podcast/list", "application/json", token, []byte(`{"v":1}`))
	if code == 200 {
		if lib, ok := parseLibraryJSON(data); ok {
			return lib
		}
	}
	jsonCode := code
	pb := cat(pbStr(1, "1"), pbStr(2, "web"))
	code, data = apiCall("/user/podcast/list", "application/octet-stream", token, pb)
	w.True(code == 200, "pocketcasts: podcast list failed: JSON form HTTP %d, protobuf form HTTP %d: %s", jsonCode, code, snippet(data))
	lib := parseLibraryProto(data)
	fillTitles(&lib)
	return lib
}

func fillTitles(lib *library) {
	parallel(len(lib.Pods), func(i int) {
		if lib.Pods[i].Title != "" {
			return
		}
		if m, err := tryMeta(lib.Pods[i].UUID); err == nil {
			lib.Pods[i].Title = m.Title
			lib.Pods[i].Latest = m.latest()
		}
	})
}

func parallel(n int, work func(i int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			work(i)
		}(i)
	}
	wg.Wait()
}

func (l library) folderName(uuid string) string {
	if uuid == "" {
		return ""
	}
	for _, f := range l.Folders {
		if f.UUID == uuid {
			return f.Name
		}
	}
	return uuid
}

func (l library) pod(uuid string) (pod, bool) {
	for _, p := range l.Pods {
		if strings.EqualFold(p.UUID, uuid) {
			return p, true
		}
	}
	return pod{}, false
}

// resolvePods turns "uuid, Some Title, other" into subscribed podcasts. A
// title may be a substring; one that matches several (and none exactly) is an
// error that lists them, never a guess.
func resolvePods(lib library, spec string) []pod {
	var res []pod
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, ok := lib.pod(item); ok {
			res = append(res, p)
			continue
		}
		var exact, partial []pod
		for _, p := range lib.Pods {
			t := strings.ToLower(p.Title)
			switch {
			case t == strings.ToLower(item):
				exact = append(exact, p)
			case strings.Contains(t, strings.ToLower(item)):
				partial = append(partial, p)
			}
		}
		cands := partial
		if len(exact) > 0 {
			cands = exact
		}
		w.True(len(cands) > 0, "pocketcasts: no subscribed podcast matches %q", item)
		if len(cands) > 1 {
			var lines []string
			for _, p := range cands {
				lines = append(lines, "  "+p.UUID+"  "+p.Title)
			}
			w.True(false, "pocketcasts: %q matches %d podcasts; use a uuid:\n%s", item, len(cands), strings.Join(lines, "\n"))
		}
		res = append(res, cands[0])
	}
	w.True(len(res) > 0, "pocketcasts: no podcast given")
	return res
}

// resolveFolder returns a folder, or ok=false for "none" (the top level).
func resolveFolder(lib library, spec string) (fold, bool) {
	spec = strings.TrimSpace(spec)
	switch strings.ToLower(spec) {
	case "", "none", "home", "top", "uncategorized":
		// A folder really named "Uncategorized" wins over the alias.
		for _, f := range lib.Folders {
			if strings.EqualFold(f.Name, spec) && spec != "" {
				return f, true
			}
		}
		return fold{}, false
	}
	for _, f := range lib.Folders {
		if strings.EqualFold(f.UUID, spec) {
			return f, true
		}
	}
	var partial []fold
	for _, f := range lib.Folders {
		if strings.EqualFold(f.Name, spec) {
			return f, true
		}
		if strings.Contains(strings.ToLower(f.Name), strings.ToLower(spec)) {
			partial = append(partial, f)
		}
	}
	w.True(len(partial) == 1, "pocketcasts: folder %q matches %d folders (use list_folders and a uuid, or \"none\")", spec, len(partial))
	return partial[0], true
}

// ---- podcast metadata and episodes ---------------------------------------

type episode struct {
	UUID, Title, Published string
	Duration               int
	Number                 int
	HasNumber              bool
	Archived               bool
	Status                 string // unplayed, in-progress, played
}

type meta struct {
	Title    string
	Episodes []episode
	Count    int
	HasMore  bool
}

func (m meta) latest() string {
	best := ""
	for _, e := range m.Episodes {
		if e.Published > best {
			best = e.Published
		}
	}
	return best
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

// tryMeta never panics, so it can run in a goroutine.
func tryMeta(uuid string) (meta, error) {
	code, data, err := publicGet(metaHost + "/podcast/full/" + url.PathEscape(uuid))
	if err != nil {
		return meta{}, err
	}
	if code != 200 {
		return meta{}, fmt.Errorf("HTTP %d: %s", code, snippet(data))
	}
	var raw struct {
		EpisodeCount int  `json:"episode_count"`
		HasMore      bool `json:"has_more_episodes"`
		Podcast      struct {
			Title    string `json:"title"`
			Episodes []struct {
				UUID      string `json:"uuid"`
				Title     string `json:"title"`
				Published string `json:"published"`
				Duration  int    `json:"duration"`
				Number    any    `json:"number"`
			} `json:"episodes"`
		} `json:"podcast"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return meta{}, err
	}
	m := meta{Title: raw.Podcast.Title, Count: raw.EpisodeCount, HasMore: raw.HasMore}
	for _, e := range raw.Podcast.Episodes {
		m.Episodes = append(m.Episodes, episode{
			UUID: e.UUID, Title: e.Title, Published: e.Published, Duration: e.Duration,
			Number: toInt(e.Number), HasNumber: toInt(e.Number) > 0,
		})
	}
	if m.Count < len(m.Episodes) {
		m.Count = len(m.Episodes)
	}
	return m, nil
}

// fetchState reads the user's per-episode state for a podcast. An episode the
// server does not mention has the defaults: not archived, unplayed.
func fetchState(token, podcast string) map[string]map[string]any {
	body := w.Check(json.Marshal(map[string]string{"uuid": podcast}))
	code, data := apiCall("/user/podcast/episodes", "application/json", token, body)
	w.True(code == 200, "pocketcasts: reading episode state failed: HTTP %d: %s", code, snippet(data))
	var raw struct {
		Episodes []map[string]any `json:"episodes"`
	}
	w.Must(json.Unmarshal(data, &raw))
	state := map[string]map[string]any{}
	for _, m := range raw.Episodes {
		state[gs(m, "uuid")] = m
	}
	return state
}

var numberPatterns = []*regexp.Regexp{
	regexp.MustCompile(`#\s*(\d+)`),
	regexp.MustCompile(`(?i)\b(?:episode|folge|ep\.?|nr\.?|no\.?)\s*(\d+)`),
	regexp.MustCompile(`^\s*(\d+)\b`),
}

// episodesOf joins the public episode list with the user's state, newest
// first, and numbers the episodes.
func episodesOf(token, podcast string) (meta, []episode) {
	m, err := tryMeta(podcast)
	w.True(err == nil, "pocketcasts: reading the episode list of %s failed: %v", podcast, err)
	state := fetchState(token, podcast)
	var custom *regexp.Regexp
	if s := opt("number_regex"); s != "" {
		c, err := regexp.Compile(s)
		w.True(err == nil, "pocketcasts: number_regex: %v", err)
		w.True(c.NumSubexp() >= 1, "pocketcasts: number_regex needs a capture group around the number")
		custom = c
	}
	eps := m.Episodes
	for i := range eps {
		e := &eps[i]
		if custom != nil {
			e.Number, e.HasNumber = 0, false
			if g := custom.FindStringSubmatch(e.Title); g != nil {
				if n, err := strconv.Atoi(g[1]); err == nil {
					e.Number, e.HasNumber = n, true
				}
			}
		} else if !e.HasNumber {
			for _, re := range numberPatterns {
				if g := re.FindStringSubmatch(e.Title); g != nil {
					if n, err := strconv.Atoi(g[1]); err == nil {
						e.Number, e.HasNumber = n, true
						break
					}
				}
			}
		}
		e.Status = "unplayed"
		if s, ok := state[e.UUID]; ok {
			e.Archived, _ = s["isDeleted"].(bool)
			switch toInt(s["playingStatus"]) {
			case 2:
				e.Status = "in-progress"
			case 3:
				e.Status = "played"
			}
		}
	}
	sort.SliceStable(eps, func(i, j int) bool { return eps[i].Published > eps[j].Published })
	return m, eps
}

type filter struct {
	from, to        int
	hasFrom, hasTo  bool
	title           *regexp.Regexp
	after, before   string
	only            string
	uuids           map[string]bool
	any             bool
}

func parseFilter() filter {
	var f filter
	if s := opt("from_number"); s != "" {
		n, err := strconv.Atoi(s)
		w.True(err == nil, "pocketcasts: from_number must be an integer, not %q", s)
		f.from, f.hasFrom, f.any = n, true, true
	}
	if s := opt("to_number"); s != "" {
		n, err := strconv.Atoi(s)
		w.True(err == nil, "pocketcasts: to_number must be an integer, not %q", s)
		f.to, f.hasTo, f.any = n, true, true
	}
	if s := opt("title_regex"); s != "" {
		re, err := regexp.Compile(s)
		w.True(err == nil, "pocketcasts: title_regex: %v", err)
		f.title, f.any = re, true
	}
	f.after, f.before = opt("published_after"), opt("published_before")
	for _, d := range []string{f.after, f.before} {
		if d != "" {
			_, err := time.Parse("2006-01-02", d)
			w.True(err == nil, "pocketcasts: dates are YYYY-MM-DD, not %q", d)
			f.any = true
		}
	}
	f.only = strings.ToLower(opt("only"))
	w.True(f.only == "" || f.only == "archived" || f.only == "unarchived", "pocketcasts: only is archived or unarchived")
	if s := opt("episodes"); s != "" {
		f.uuids = map[string]bool{}
		for _, u := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			f.uuids[strings.ToLower(u)] = true
		}
		f.any = true
	}
	return f
}

func (f filter) match(e episode) bool {
	if f.hasFrom && (!e.HasNumber || e.Number < f.from) {
		return false
	}
	if f.hasTo && (!e.HasNumber || e.Number > f.to) {
		return false
	}
	if f.title != nil && !f.title.MatchString(e.Title) {
		return false
	}
	day := e.Published
	if len(day) > 10 {
		day = day[:10]
	}
	if f.after != "" && day < f.after {
		return false
	}
	if f.before != "" && day >= f.before {
		return false
	}
	if f.only == "archived" && !e.Archived {
		return false
	}
	if f.only == "unarchived" && e.Archived {
		return false
	}
	if f.uuids != nil && !f.uuids[strings.ToLower(e.UUID)] {
		return false
	}
	return true
}

func date(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func episodeRow(e episode) string {
	n := ""
	if e.HasNumber {
		n = strconv.Itoa(e.Number)
	}
	state := e.Status
	if e.Archived {
		state = "archived"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s", n, date(e.Published), state, e.UUID, e.Title)
}

// ---- operations ----------------------------------------------------------

func opSearch() {
	q := required("query")
	body := w.Check(json.Marshal(map[string]string{"term": q}))
	req := w.Check(http.NewRequest("POST", metaHost+"/discover/search", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "caos-pocketcasts")
	resp, err := pubClient.Do(req)
	w.True(err == nil, "pocketcasts: search got no response: %v", err)
	defer resp.Body.Close()
	data := w.Check(io.ReadAll(io.LimitReader(resp.Body, 8<<20)))
	w.True(resp.StatusCode == 200, "pocketcasts: search failed: HTTP %d: %s (this endpoint is unverified; the podcast uuid is also in a podcast's pca.st / pocketcasts.com share URL)", resp.StatusCode, snippet(data))
	var raw struct {
		Result struct {
			Podcasts []map[string]any `json:"podcasts"`
		} `json:"result"`
		Podcasts []map[string]any `json:"podcasts"`
	}
	w.Must(json.Unmarshal(data, &raw))
	list := append(raw.Result.Podcasts, raw.Podcasts...)
	pf("# %d results for %q\nuuid\ttitle\tauthor\n", len(list), q)
	for _, m := range list {
		pf("%s\t%s\t%s\n", gs(m, "uuid"), gs(m, "title"), gs(m, "author"))
	}
}

func sortedPods(pods []pod) []pod {
	pods = append([]pod(nil), pods...)
	sort.SliceStable(pods, func(i, j int) bool {
		return strings.ToLower(pods[i].Title) < strings.ToLower(pods[j].Title)
	})
	return pods
}

func opListPodcasts(token string) {
	lib := fetchLibrary(token)
	pods := lib.Pods
	if s := opt("folder"); s != "" {
		f, ok := resolveFolder(lib, s)
		want := ""
		if ok {
			want = f.UUID
		}
		var keep []pod
		for _, p := range pods {
			if p.Folder == want {
				keep = append(keep, p)
			}
		}
		pods = keep
	}
	pods = sortedPods(pods)
	withLatest := truthy(opt("with-latest"))
	metas := make([]meta, len(pods))
	errs := make([]error, len(pods))
	if withLatest {
		parallel(len(pods), func(i int) { metas[i], errs[i] = tryMeta(pods[i].UUID) })
	}
	pf("# %d podcasts (folder \"\" = not in a folder)\n", len(pods))
	if withLatest {
		pf("uuid\tfolder\ttitle\tlatest_episode\tepisodes\n")
	} else {
		pf("uuid\tfolder\ttitle\tlatest_episode(as reported by the list, may be empty)\n")
	}
	for i, p := range pods {
		latest, count := date(p.Latest), ""
		if withLatest {
			if errs[i] != nil {
				latest = "? (" + errs[i].Error() + ")"
			} else {
				latest, count = date(metas[i].latest()), strconv.Itoa(metas[i].Count)
			}
		}
		if withLatest {
			pf("%s\t%s\t%s\t%s\t%s\n", p.UUID, lib.folderName(p.Folder), p.Title, latest, count)
		} else {
			pf("%s\t%s\t%s\t%s\n", p.UUID, lib.folderName(p.Folder), p.Title, latest)
		}
	}
}

func opListFolders(token string) {
	lib := fetchLibrary(token)
	counts := map[string]int{}
	for _, p := range lib.Pods {
		counts[p.Folder]++
	}
	pf("# %d folders; %d podcasts are not in any folder\nuuid\tname\tpodcasts\n", len(lib.Folders), counts[""])
	for _, f := range lib.Folders {
		pf("%s\t%s\t%d\n", f.UUID, f.Name, counts[f.UUID])
	}
}

func opSubscribe(token string) {
	lib := fetchLibrary(token)
	var target *fold
	if s := opt("folder"); s != "" {
		if f, ok := resolveFolder(lib, s); ok {
			target = &f
		}
	}
	now := time.Now()
	var recs [][]byte
	var done []string
	yes := true
	for _, u := range strings.Split(required("podcast"), ",") {
		u = strings.TrimSpace(u)
		w.True(uuidRe.MatchString(u), "pocketcasts: subscribe takes podcast uuids (use search to find one), not %q", u)
		if _, ok := lib.pod(u); ok {
			pf("already subscribed: %s\n", u)
			continue
		}
		var folder *string
		if target != nil {
			folder = &target.UUID
		}
		recs = append(recs, podcastRec(u, &yes, folder, &now))
		done = append(done, u)
	}
	if len(recs) == 0 {
		return
	}
	syncUpdate(token, recs)
	after := fetchLibrary(token)
	for _, u := range done {
		if p, ok := after.pod(u); ok {
			pf("subscribed: %s  %s\n", u, p.Title)
		} else {
			pf("NOT CONFIRMED: %s is not in the library yet; re-read with list_podcasts before retrying\n", u)
		}
	}
}

func opUnsubscribe(token string) {
	lib := fetchLibrary(token)
	pods := resolvePods(lib, required("podcast"))
	pf("%d podcasts to unsubscribe from:\n", len(pods))
	for _, p := range pods {
		pf("%s\t%s\t%s\n", p.UUID, lib.folderName(p.Folder), p.Title)
	}
	if !truthy(opt("apply")) {
		pf("\nDRY RUN: nothing changed. Confirm the list with the user, then re-run with apply=true.\n")
		return
	}
	no := false
	var recs [][]byte
	for _, p := range pods {
		recs = append(recs, podcastRec(p.UUID, &no, nil, nil))
	}
	syncUpdate(token, recs)
	after := fetchLibrary(token)
	left := 0
	for _, p := range pods {
		if _, ok := after.pod(p.UUID); ok {
			left++
			pf("STILL LISTED: %s  %s\n", p.UUID, p.Title)
		}
	}
	pf("\nunsubscribed %d of %d\n", len(pods)-left, len(pods))
}

func opMovePodcast(token string) {
	lib := fetchLibrary(token)
	pods := resolvePods(lib, required("podcast"))
	f, ok := resolveFolder(lib, required("folder"))
	target := homeFolder
	label := "(no folder)"
	if ok {
		target, label = f.UUID, f.Name
	}
	var recs [][]byte
	for _, p := range pods {
		recs = append(recs, podcastRec(p.UUID, nil, &target, nil))
	}
	syncUpdate(token, recs)
	after := fetchLibrary(token)
	want := noHome(target)
	for _, p := range pods {
		if q, ok := after.pod(p.UUID); ok && q.Folder == want {
			pf("moved to %s: %s\n", label, p.Title)
		} else {
			pf("NOT CONFIRMED: %s (%s) is not in %s yet\n", p.Title, p.UUID, label)
		}
	}
}

func opCreateFolder(token string) {
	lib := fetchLibrary(token)
	name := required("name")
	for _, f := range lib.Folders {
		w.True(!strings.EqualFold(f.Name, name), "pocketcasts: a folder named %q already exists (%s)", f.Name, f.UUID)
	}
	f := fold{UUID: newUUID(), Name: name}
	if s := opt("color"); s != "" {
		f.Color = w.Check(strconv.Atoi(s))
	}
	for _, g := range lib.Folders {
		f.SortPos = max(f.SortPos, g.SortPos)
	}
	f.SortPos++
	now := time.Now()
	syncUpdate(token, [][]byte{folderRec(f, false, &now)})
	after := fetchLibrary(token)
	for _, g := range after.Folders {
		if g.UUID == f.UUID {
			pf("created folder %s  %s\n", g.UUID, g.Name)
			return
		}
	}
	pf("NOT CONFIRMED: folder %q (%s) is not listed yet; run list_folders before retrying\n", name, f.UUID)
}

func opRenameFolder(token string) {
	lib := fetchLibrary(token)
	f, ok := resolveFolder(lib, required("folder"))
	w.True(ok, "pocketcasts: say which folder to rename")
	f.Name = required("name")
	syncUpdate(token, [][]byte{folderRec(f, false, nil)})
	after := fetchLibrary(token)
	for _, g := range after.Folders {
		if g.UUID == f.UUID && g.Name == f.Name {
			pf("renamed folder %s to %q\n", g.UUID, g.Name)
			return
		}
	}
	pf("NOT CONFIRMED: folder %s is not named %q yet\n", f.UUID, f.Name)
}

func opDeleteFolder(token string) {
	lib := fetchLibrary(token)
	f, ok := resolveFolder(lib, required("folder"))
	w.True(ok, "pocketcasts: say which folder to delete")
	var inside []pod
	for _, p := range lib.Pods {
		if p.Folder == f.UUID {
			inside = append(inside, p)
		}
	}
	pf("delete folder %q (%s); its %d podcasts move out to the top level, still subscribed:\n", f.Name, f.UUID, len(inside))
	for _, p := range inside {
		pf("  %s\t%s\n", p.UUID, p.Title)
	}
	if !truthy(opt("apply")) {
		pf("\nDRY RUN: nothing changed. Re-run with apply=true to delete the folder.\n")
		return
	}
	var recs [][]byte
	home := homeFolder
	for _, p := range inside {
		recs = append(recs, podcastRec(p.UUID, nil, &home, nil))
	}
	recs = append(recs, folderRec(f, true, nil))
	syncUpdate(token, recs)
	after := fetchLibrary(token)
	for _, g := range after.Folders {
		if g.UUID == f.UUID {
			pf("NOT CONFIRMED: folder %s is still listed\n", f.UUID)
			return
		}
	}
	pf("\ndeleted folder %q\n", f.Name)
}

func opListEpisodes(token string) {
	lib := fetchLibrary(token)
	pods := resolvePods(lib, required("podcast"))
	w.True(len(pods) == 1, "pocketcasts: list_episodes takes one podcast")
	m, eps := episodesOf(token, pods[0].UUID)
	f := parseFilter()
	var sel []episode
	archived := 0
	for _, e := range eps {
		if f.match(e) {
			sel = append(sel, e)
			if e.Archived {
				archived++
			}
		}
	}
	limit := 200
	if s := opt("max_rows"); s != "" {
		limit = w.Check(strconv.Atoi(s))
	}
	pf("# %s (%s)\n# %d of %d known episodes match; %d of those archived\n", pods[0].Title, pods[0].UUID, len(sel), len(eps), archived)
	coverage(m, eps)
	pf("number\tpublished\tstate\tuuid\ttitle\n")
	for i, e := range sel {
		if i == limit {
			pf("[%d more rows not shown; raise max_rows or narrow the filter]\n", len(sel)-limit)
			break
		}
		pf("%s\n", episodeRow(e))
	}
}

// coverage warns when the public episode list is shorter than the podcast.
func coverage(m meta, eps []episode) {
	if m.HasMore || m.Count > len(eps) {
		pf("# WARNING: the public episode list has %d of %d episodes (has_more_episodes=%v); older episodes are not visible to this tool.\n", len(eps), m.Count, m.HasMore)
	}
}

func opArchive(token string, archived bool) {
	verb := map[bool]string{true: "archive", false: "unarchive"}[archived]
	lib := fetchLibrary(token)
	pods := resolvePods(lib, required("podcast"))
	w.True(len(pods) == 1, "pocketcasts: %s takes one podcast", verb)
	p := pods[0]
	f := parseFilter()
	w.True(f.any || truthy(opt("all")), "pocketcasts: refusing to %s every episode by default: add a filter (from_number/to_number, title_regex, published_after/published_before, episodes) or all=true", verb)
	m, eps := episodesOf(token, p.UUID)
	var matched, todo []episode
	for _, e := range eps {
		if f.match(e) {
			matched = append(matched, e)
			if e.Archived != archived {
				todo = append(todo, e)
			}
		}
	}
	pf("%s: %s (%s)\n", verb, p.Title, p.UUID)
	coverage(m, eps)
	pf("%d episodes match; %d already %sd; %d to change\n", len(matched), len(matched)-len(todo), verb, len(todo))
	if len(todo) > 0 {
		lo, hi, withNumber := 0, 0, 0
		for _, e := range todo {
			if e.HasNumber {
				if withNumber == 0 || e.Number < lo {
					lo = e.Number
				}
				if withNumber == 0 || e.Number > hi {
					hi = e.Number
				}
				withNumber++
			}
		}
		if withNumber > 0 {
			pf("numbers %d-%d (%d of the %d have a number; %d do not)\n", lo, hi, withNumber, len(todo), len(todo)-withNumber)
		} else {
			pf("none of these episodes has a number\n")
		}
		pf("number\tpublished\tstate\tuuid\ttitle\n")
		show := todo
		if len(show) > 12 {
			show = append(append([]episode(nil), todo[:6]...), todo[len(todo)-6:]...)
		}
		for i, e := range show {
			if len(todo) > 12 && i == 6 {
				pf("... %d more ...\n", len(todo)-12)
			}
			pf("%s\n", episodeRow(e))
		}
	}
	if !truthy(opt("apply")) {
		pf("\nDRY RUN: nothing changed. Check the rows above (episode numbers are parsed from titles unless the feed provides them), then re-run with apply=true.\n")
		return
	}
	if len(todo) == 0 {
		pf("\nnothing to do\n")
		return
	}
	now := time.Now().UnixMilli()
	var recs [][]byte
	for _, e := range todo {
		recs = append(recs, episodeRec(p.UUID, e.UUID, archived, now))
	}
	syncUpdate(token, recs)
	state := fetchState(token, p.UUID)
	wrong := 0
	for _, e := range todo {
		got, _ := state[e.UUID]["isDeleted"].(bool)
		if got != archived {
			wrong++
		}
	}
	if wrong > 0 {
		pf("\nNOT CONFIRMED: %d of %d episodes do not read back as %sd; re-read with list_episodes before retrying\n", wrong, len(todo), verb)
	} else {
		pf("\n%sd %d episodes (read back and confirmed)\n", verb, len(todo))
	}
}

// opSelftest checks the protobuf encoder against hand-computed bytes. It needs
// no credentials and no network.
func opSelftest() {
	yes, no := true, false
	home := "x"
	cases := []struct {
		name string
		got  []byte
		want string
	}{
		{"varint 300", binary.AppendUvarint(nil, 300), "ac02"},
		{"subscribe", podcastRec("ab", &yes, nil, nil), "0a080a026162" + "1a020801"},
		{"unsubscribe (empty BoolValue)", podcastRec("ab", &no, nil, nil), "0a060a026162" + "1a00"},
		{"folder wrapper", podcastRec("ab", nil, &home, nil), "0a09" + "0a026162" + "3a030a0178"},
		// uuid "e", podcast_uuid "p", is_deleted true, is_deleted_modified 1
		{"episode archived", episodeRec("p", "e", true, 1), "120e" + "0a0165" + "120170" + "1a020801" + "22020801"},
		// Record.folder (field 5): folder_uuid "f", is_deleted true
		{"folder delete", folderRec(fold{UUID: "f"}, true, nil), "2a05" + "0a0166" + "1001"},
	}
	bad := 0
	for _, c := range cases {
		got := hex.EncodeToString(c.got)
		if got == c.want {
			pf("ok    %s\n", c.name)
		} else {
			bad++
			pf("FAIL  %s\n  got  %s\n  want %s\n", c.name, got, c.want)
		}
	}
	roundtrip := pbParse(cat(pbStr(1, "u"), pbVarint(3, 7)))
	if len(roundtrip) == 2 && string(roundtrip[0].b) == "u" && roundtrip[1].v == 7 {
		pf("ok    parse round trip\n")
	} else {
		bad++
		pf("FAIL  parse round trip: %+v\n", roundtrip)
	}
	w.True(bad == 0, "pocketcasts: selftest: %d failures\n%s", bad, out.String())
}

func main() {
	w.Main(func() {
		required("call")
		op := required("op")
		switch op {
		case "selftest":
			opSelftest()
		case "search":
			opSearch()
		case "list_podcasts", "list_folders", "subscribe", "unsubscribe", "move_podcast",
			"create_folder", "rename_folder", "delete_folder", "list_episodes", "archive", "unarchive":
			token := login()
			switch op {
			case "list_podcasts":
				opListPodcasts(token)
			case "list_folders":
				opListFolders(token)
			case "subscribe":
				opSubscribe(token)
			case "unsubscribe":
				opUnsubscribe(token)
			case "move_podcast":
				opMovePodcast(token)
			case "create_folder":
				opCreateFolder(token)
			case "rename_folder":
				opRenameFolder(token)
			case "delete_folder":
				opDeleteFolder(token)
			case "list_episodes":
				opListEpisodes(token)
			case "archive":
				opArchive(token, true)
			case "unarchive":
				opArchive(token, false)
			}
		default:
			w.True(false, "pocketcasts: unknown op %q; see tool_help", op)
		}
		body := out.String()
		if len(body) > maxBody {
			cut := maxBody
			for cut > 0 && body[cut]&0xC0 == 0x80 {
				cut--
			}
			body = body[:cut] + fmt.Sprintf("\n[truncated after %d bytes; narrow the query]", cut)
		}
		w.Report(body)
	})
}
