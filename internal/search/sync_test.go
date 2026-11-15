package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// fakeMeili is the subset of the Meilisearch HTTP API the client uses, kept
// in memory. down makes every request fail, like a stopped service.
type fakeMeili struct {
	mu   sync.Mutex
	docs map[int64]IndexDocument
	down bool
}

func newFakeMeili(t *testing.T) (*fakeMeili, *Client) {
	t.Helper()
	f := &fakeMeili{docs: map[int64]IndexDocument{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, New(&config.MeilisearchConfig{Host: srv.URL, APIKey: "test"})
}

func (f *fakeMeili) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

func (f *fakeMeili) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	const docsPath = "/indexes/messages/documents"
	switch {
	case r.URL.Path == "/health", r.URL.Path == "/indexes", strings.HasPrefix(r.URL.Path, "/indexes/messages/settings/"):
		w.WriteHeader(http.StatusAccepted)
	case r.Method == "POST" && r.URL.Path == docsPath:
		var docs []IndexDocument
		if err := json.NewDecoder(r.Body).Decode(&docs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, d := range docs {
			f.docs[d.ID] = d
		}
		w.WriteHeader(http.StatusAccepted)
	case r.Method == "POST" && r.URL.Path == docsPath+"/delete-batch":
		var ids []int64
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, id := range ids {
			delete(f.docs, id)
		}
		w.WriteHeader(http.StatusAccepted)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, docsPath+"/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, docsPath+"/"), 10, 64)
		delete(f.docs, id)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == "GET" && r.URL.Path == docsPath:
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		ids := make([]int64, 0, len(f.docs))
		for id := range f.docs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		type row struct {
			ID int64 `json:"id"`
		}
		res := []row{}
		for i := offset; i < len(ids) && i < offset+limit; i++ {
			res = append(res, row{ids[i]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": res, "total": len(ids)})
	case r.Method == "POST" && r.URL.Path == "/indexes/messages/search":
		var req struct {
			Q      string `json:"q"`
			Filter string `json:"filter"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var userID int64
		if _, err := parseUserFilter(req.Filter, &userID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hits := []IndexDocument{}
		q := strings.ToLower(req.Q)
		for _, d := range f.docs {
			if d.UserID != userID || d.SoftDeleted {
				continue
			}
			if strings.Contains(strings.ToLower(d.Subject+" "+d.Body), q) {
				hits = append(hits, d)
			}
		}
		_ = json.NewEncoder(w).Encode(SearchResult{Hits: hits, Query: req.Q})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func parseUserFilter(filter string, userID *int64) (int, error) {
	// "user_id = N AND ..."
	f := strings.Fields(filter)
	if len(f) < 3 {
		return 0, strconv.ErrSyntax
	}
	v, err := strconv.ParseInt(f[2], 10, 64)
	*userID = v
	return 1, err
}

// fakeStore keeps messages in memory and enqueues every change like the
// search_index_queue trigger of migration 060 does.
type fakeStore struct {
	mu     sync.Mutex
	nextID int64
	seq    int64
	msgs   map[int64]*models.Message
	queue  map[int64]int64 // message row id → seq
}

func newFakeStore() *fakeStore {
	return &fakeStore{msgs: map[int64]*models.Message{}, queue: map[int64]int64{}}
}

func (s *fakeStore) enqueue(id int64) {
	s.seq++
	s.queue[id] = s.seq
}

func (s *fakeStore) save(m *models.Message) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == 0 {
		s.nextID++
		m.ID = s.nextID
	}
	cp := *m
	s.msgs[m.ID] = &cp
	s.enqueue(m.ID)
	return m.ID
}

func (s *fakeStore) remove(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.msgs, id)
	s.enqueue(id)
}

func (s *fakeStore) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *fakeStore) GetMessageByID(id int64) (*models.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[id], nil
}

func (s *fakeStore) GetMessagesForIndexing(afterID int64, limit int) ([]*models.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id := range s.msgs {
		if id > afterID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []*models.Message
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		out = append(out, s.msgs[id])
	}
	return out, nil
}

func (s *fakeStore) GetMessagesForIndexingByIDs(ids []int64) ([]*models.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*models.Message
	for _, id := range ids {
		if m, ok := s.msgs[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *fakeStore) PendingSearchIndex(limit int) ([]db.SearchIndexEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []db.SearchIndexEntry
	for id, seq := range s.queue {
		out = append(out, db.SearchIndexEntry{MessageRowID: id, Seq: seq})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) AckSearchIndex(entries []db.SearchIndexEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if s.queue[e.MessageRowID] == e.Seq {
			delete(s.queue, e.MessageRowID)
		}
	}
	return nil
}

func (s *fakeStore) MessageRowsExist(ids []int64) (map[int64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]bool{}
	for _, id := range ids {
		if _, ok := s.msgs[id]; ok {
			out[id] = true
		}
	}
	return out, nil
}

func startIndexer(t *testing.T, client *Client, st *fakeStore) *Indexer {
	t.Helper()
	idx := &Indexer{client: client, database: st, pollInterval: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { idx.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return idx
}

func hitIDs(t *testing.T, idx *Indexer, userID int64, q string) []int64 {
	t.Helper()
	res, err := idx.Search(userID, q, 50, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	ids := []int64{}
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	return ids
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The bug: the index was filled only by the full reindex at start, so a
// message saved afterwards was not found until the next restart.
func TestRun_MessageSavedAfterStartIsSearchable(t *testing.T) {
	meili, client := newFakeMeili(t)
	st := newFakeStore()
	old := st.save(&models.Message{UserID: 7, FolderID: 1, Subject: "старое письмо", Body: "архив"})

	idx := startIndexer(t, client, st)
	waitFor(t, "start-up indexing", func() bool { return len(hitIDs(t, idx, 7, "архив")) == 1 && st.queued() == 0 })

	fresh := st.save(&models.Message{UserID: 7, FolderID: 1, Subject: "Квартальный отчёт", Body: "цифры за ноябрь", Date: time.Now().UnixMilli()})
	waitFor(t, "fresh message in search", func() bool {
		ids := hitIDs(t, idx, 7, "квартальный")
		return len(ids) == 1 && ids[0] == fresh
	})
	if ids := hitIDs(t, idx, 8, "квартальный"); len(ids) != 0 {
		t.Fatalf("other user sees %v", ids)
	}

	// Move to another folder: the document follows.
	st.save(&models.Message{ID: fresh, UserID: 7, FolderID: 2, Subject: "Квартальный отчёт", Body: "цифры за ноябрь"})
	waitFor(t, "moved document", func() bool {
		meili.mu.Lock()
		defer meili.mu.Unlock()
		return meili.docs[fresh].FolderID == 2
	})

	// Delete: the document goes away.
	st.remove(old)
	waitFor(t, "deleted message gone from search", func() bool { return len(hitIDs(t, idx, 7, "архив")) == 0 })
}

// Meilisearch down: delivery is not affected, changes wait in the queue and
// reach the index once it is back.
func TestRun_QueueSurvivesIndexOutage(t *testing.T) {
	meili, client := newFakeMeili(t)
	st := newFakeStore()
	idx := startIndexer(t, client, st)
	waitFor(t, "indexer ready", func() bool { _, err := idx.Search(1, "x", 1, 0); return err == nil })

	meili.setDown(true)
	id := st.save(&models.Message{UserID: 1, Subject: "во время простоя"})
	time.Sleep(100 * time.Millisecond)
	if st.queued() != 1 {
		t.Fatalf("queue = %d while index is down, want 1", st.queued())
	}
	meili.setDown(false)
	waitFor(t, "message indexed after outage", func() bool {
		ids := hitIDs(t, idx, 1, "простоя")
		return len(ids) == 1 && ids[0] == id && st.queued() == 0
	})
}

// A change made while the worker holds an entry keeps its newer entry.
func TestSyncPending_KeepsEntryChangedMeanwhile(t *testing.T) {
	_, client := newFakeMeili(t)
	st := newFakeStore()
	idx := &Indexer{client: client, database: st}
	id := st.save(&models.Message{UserID: 1, Subject: "v1"})
	entries, _ := st.PendingSearchIndex(10)
	st.save(&models.Message{ID: id, UserID: 1, Subject: "v2"}) // new seq
	if err := st.AckSearchIndex(entries); err != nil {
		t.Fatal(err)
	}
	if st.queued() != 1 {
		t.Fatalf("newer change lost from the queue")
	}
	if n, err := idx.SyncPending(10); err != nil || n != 1 {
		t.Fatalf("SyncPending = %d, %v", n, err)
	}
	if ids := hitIDs(t, idx, 1, "v2"); len(ids) != 1 {
		t.Fatalf("v2 not indexed: %v", ids)
	}
}

// Documents of messages deleted before the queue existed are dropped at start.
func TestReconcile_RemovesGhostDocuments(t *testing.T) {
	meili, client := newFakeMeili(t)
	st := newFakeStore()
	live := st.save(&models.Message{UserID: 1, Subject: "живое"})
	meili.docs[live] = IndexDocument{ID: live, UserID: 1, Subject: "живое"}
	meili.docs[999] = IndexDocument{ID: 999, UserID: 1, Subject: "призрак"}

	idx := &Indexer{client: client, database: st}
	n, err := idx.Reconcile()
	if err != nil || n != 1 {
		t.Fatalf("Reconcile = %d, %v; want 1", n, err)
	}
	if _, ok := meili.docs[999]; ok {
		t.Fatal("ghost document kept")
	}
	if _, ok := meili.docs[live]; !ok {
		t.Fatal("live document removed")
	}
}
