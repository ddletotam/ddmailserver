package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/gorilla/mux"
)

// memTaskStore — календари, задачи и очередь обратной синхронизации в памяти.
type memTaskStore struct {
	mu     sync.Mutex
	cals   map[int64]*models.Calendar
	events map[int64]*models.CalendarEvent
	nextID int64
	// queue — event_id → операция, как UNIQUE(event_id) у настоящей очереди:
	// новая запись по той же задаче заменяет старую.
	queue    map[int64]queuedSync
	failNext bool
}

type queuedSync struct {
	op, uid, remoteID, ical string
}

func newMemTaskStore(cals ...*models.Calendar) *memTaskStore {
	m := &memTaskStore{cals: map[int64]*models.Calendar{}, events: map[int64]*models.CalendarEvent{}, queue: map[int64]queuedSync{}, nextID: 100}
	for _, c := range cals {
		m.cals[c.ID] = c
	}
	return m
}

func (m *memTaskStore) GetEnabledCalendarsByUserID(userID int64) ([]*models.Calendar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.Calendar
	for _, c := range m.cals {
		if c.UserID == userID && c.Enabled {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memTaskStore) GetCalendarByID(id int64) (*models.Calendar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cals[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, fmt.Errorf("calendar not found")
}

func (m *memTaskStore) GetTodosForCalendars(ids []int64, includeCompleted bool) ([]*models.CalendarEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []*models.CalendarEvent
	for _, e := range m.events {
		if want[e.CalendarID] && e.IsTodo() && (includeCompleted || !e.IsCompleted()) {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memTaskStore) GetEventByID(id int64) (*models.CalendarEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.events[id]; ok {
		cp := *e
		return &cp, nil
	}
	return nil, fmt.Errorf("event not found")
}

func (m *memTaskStore) CreateCalendarEvent(e *models.CalendarEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return fmt.Errorf("db down")
	}
	m.nextID++
	e.ID = m.nextID
	cp := *e
	m.events[e.ID] = &cp
	return nil
}

func (m *memTaskStore) UpdateCalendarEvent(e *models.CalendarEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[e.ID]; !ok {
		return fmt.Errorf("event not found")
	}
	cp := *e
	m.events[e.ID] = &cp
	return nil
}

func (m *memTaskStore) DeleteCalendarEvent(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.events, id)
	return nil
}

func (m *memTaskStore) QueueCalendarEventSync(eventID, calendarID, sourceID int64, uid, remoteID, icalData, operation string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue[eventID] = queuedSync{op: operation, uid: uid, remoteID: remoteID, ical: icalData}
	return nil
}

func (m *memTaskStore) event(id int64) *models.CalendarEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events[id]
}

const taskUserID = 7

// Коллекции: iCloud-список напоминаний (VTODO, обратная синхронизация),
// календарь событий, подписка ICS, чужой список и выключенный список.
func taskFixtures() *memTaskStore {
	return newMemTaskStore(
		&models.Calendar{ID: 1, UserID: taskUserID, SourceID: 10, Name: "Напоминания", Color: "#f80", CanWrite: true, Enabled: true, ReverseSync: true, SourceType: "caldav", RemoteID: "/123/calendars/tasks/", SupportedComponents: "VTODO", IdentityEmail: "me@icloud.com"},
		&models.Calendar{ID: 2, UserID: taskUserID, SourceID: 10, Name: "Дом", CanWrite: true, Enabled: true, ReverseSync: true, SourceType: "caldav", SupportedComponents: "VEVENT"},
		&models.Calendar{ID: 3, UserID: taskUserID, SourceID: 11, Name: "Праздники", CanWrite: false, Enabled: true, SourceType: "ics_url", SupportedComponents: "VEVENT,VTODO"},
		&models.Calendar{ID: 4, UserID: 99, SourceID: 12, Name: "Чужой", CanWrite: true, Enabled: true, SourceType: "caldav", SupportedComponents: "VTODO"},
		&models.Calendar{ID: 5, UserID: taskUserID, SourceID: 10, Name: "Выключенный", CanWrite: true, Enabled: false, ReverseSync: true, SourceType: "caldav", SupportedComponents: "VTODO"},
	)
}

// taskRouter — те же пути и методы, что в setupRoutes, без PostgreSQL и
// без проверки токена (пользователь кладётся в контекст запроса).
func taskRouter(store taskStore) *mux.Router {
	a := taskAPI{store: store}
	r := mux.NewRouter()
	r.HandleFunc("/api/desktop/v1/tasks", a.list).Methods("GET")
	r.HandleFunc("/api/desktop/v1/tasks", a.create).Methods("POST")
	r.HandleFunc("/api/desktop/v1/tasks/{id}/completion", a.completion).Methods("POST")
	r.HandleFunc("/api/desktop/v1/tasks/{id}", a.patch).Methods("PATCH")
	r.HandleFunc("/api/desktop/v1/tasks/{id}", a.remove).Methods("DELETE")
	return r
}

func taskCall(t *testing.T, store taskStore, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(setUserContext(req.Context(), &models.User{ID: taskUserID, Username: "alice"}))
	rec := httptest.NewRecorder()
	taskRouter(store).ServeHTTP(rec, req)
	return rec
}

func decodeTask(t *testing.T, rec *httptest.ResponseRecorder) DesktopTask {
	t.Helper()
	var task DesktopTask
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("ответ не задача: %v: %s", err, rec.Body)
	}
	return task
}

func TestTaskCreateQueuesPutUpstream(t *testing.T) {
	store := taskFixtures()
	rec := taskCall(t, store, "POST", "/api/desktop/v1/tasks",
		`{"calendar_id":1,"summary":"  Оплатить офис  ","description":"до пятницы","due":1794562200000,"all_day":true,"priority":1}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	task := decodeTask(t, rec)
	if task.Summary != "Оплатить офис" || task.Description != "до пятницы" {
		t.Errorf("поля: %+v", task)
	}
	// Срок датой — полночь UTC той же даты.
	if task.Due == nil || *task.Due != 1794528000000 || !task.AllDay {
		t.Errorf("срок: %v all_day=%t", task.Due, task.AllDay)
	}
	if task.Priority == nil || *task.Priority != 1 {
		t.Errorf("приоритет: %v", task.Priority)
	}
	if !task.CanWrite || task.CalendarName != "Напоминания" || task.Color != "#f80" || task.Completed || task.Status != "NEEDS-ACTION" {
		t.Errorf("поля коллекции/статуса: %+v", task)
	}
	if !strings.HasSuffix(task.UID, "@ddmailserver") {
		t.Errorf("UID = %q, ждали …@ddmailserver", task.UID)
	}

	row := store.event(task.ID)
	if row == nil || row.Component != models.ComponentTodo || !row.LocalModified {
		t.Fatalf("строка: %+v", row)
	}
	if !strings.Contains(row.ICalData, "BEGIN:VTODO") || !strings.Contains(row.ICalData, "DUE;VALUE=DATE:20261113") {
		t.Errorf("тело задачи:\n%s", row.ICalData)
	}

	q, ok := store.queue[task.ID]
	if !ok || q.op != "create" || q.uid != task.UID || q.ical != row.ICalData {
		t.Errorf("очередь обратной синхронизации: %+v", q)
	}

	// И задача видна в списке.
	list := taskCall(t, store, "GET", "/api/desktop/v1/tasks", "")
	var tasks []DesktopTask
	if err := json.Unmarshal(list.Body.Bytes(), &tasks); err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Errorf("список: %s", list.Body)
	}
}

func TestTaskCreateWithoutDue(t *testing.T) {
	store := taskFixtures()
	rec := taskCall(t, store, "POST", "/api/desktop/v1/tasks", `{"calendar_id":1,"summary":"Без срока","all_day":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	task := decodeTask(t, rec)
	if task.Due != nil || task.AllDay || task.Priority != nil {
		t.Errorf("лишнее у задачи без срока: %+v", task)
	}
	if strings.Contains(store.event(task.ID).ICalData, "DUE") {
		t.Error("в теле есть DUE")
	}
}

func TestTaskCreateRejects(t *testing.T) {
	cases := []struct {
		name, body string
		code       int
	}{
		{"без заголовка", `{"calendar_id":1,"summary":"   "}`, http.StatusBadRequest},
		{"без коллекции", `{"summary":"x"}`, http.StatusBadRequest},
		{"кривой JSON", `{"summary":`, http.StatusBadRequest},
		{"приоритет вне 0..9", `{"calendar_id":1,"summary":"x","priority":12}`, http.StatusBadRequest},
		{"коллекция событий", `{"calendar_id":2,"summary":"x"}`, http.StatusUnprocessableEntity},
		{"подписка ICS", `{"calendar_id":3,"summary":"x"}`, http.StatusForbidden},
		{"чужая коллекция", `{"calendar_id":4,"summary":"x"}`, http.StatusNotFound},
		{"выключенная коллекция", `{"calendar_id":5,"summary":"x"}`, http.StatusNotFound},
		{"нет такой", `{"calendar_id":777,"summary":"x"}`, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := taskFixtures()
			rec := taskCall(t, store, "POST", "/api/desktop/v1/tasks", c.body)
			if rec.Code != c.code {
				t.Errorf("код %d, ждали %d: %s", rec.Code, c.code, rec.Body)
			}
			if len(store.events) != 0 || len(store.queue) != 0 {
				t.Error("отказ не должен ничего создавать")
			}
		})
	}
}

func TestTaskCreateStoreFailure(t *testing.T) {
	store := taskFixtures()
	store.failNext = true
	rec := taskCall(t, store, "POST", "/api/desktop/v1/tasks", `{"calendar_id":1,"summary":"x"}`)
	if rec.Code != http.StatusInternalServerError || len(store.queue) != 0 {
		t.Errorf("код %d, очередь %v", rec.Code, store.queue)
	}
}

// Задача, пришедшая из iCloud: правка должна сохранить свойства Apple и уйти
// наверх как update по её remote_id.
func seedAppleTask(store *memTaskStore) int64 {
	due := int64(1789808400000) // 2026-09-19T09:00:00Z
	prio := int16(5)
	e := &models.CalendarEvent{
		CalendarID: 1, UID: "FCC55C22", RemoteID: "/123/calendars/tasks/FCC55C22.ics", Component: models.ComponentTodo,
		Summary: "Оплата офиса", Description: "старая", Due: &due, DTStart: due, Priority: &prio, Status: "NEEDS-ACTION",
		ICalData: "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:FCC55C22\r\nDTSTAMP:20260825T143443Z\r\n" +
			"DTSTART:20260919T090000Z\r\nDUE:20260919T090000Z\r\nSUMMARY:Оплата офиса\r\nDESCRIPTION:старая\r\nPRIORITY:5\r\n" +
			"STATUS:NEEDS-ACTION\r\nX-APPLE-SORT-ORDER:12\r\nEND:VTODO\r\nEND:VCALENDAR\r\n",
	}
	if err := store.CreateCalendarEvent(e); err != nil {
		panic(err)
	}
	return e.ID
}

func TestTaskPatchRewritesFieldsAndQueuesUpdate(t *testing.T) {
	store := taskFixtures()
	id := seedAppleTask(store)

	rec := taskCall(t, store, "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", id),
		`{"summary":"Оплата офиса и склада","due":1794562200000,"all_day":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	task := decodeTask(t, rec)
	if task.Summary != "Оплата офиса и склада" || task.Description != "старая" {
		t.Errorf("поля: %+v", task)
	}
	if task.Due == nil || *task.Due != 1794562200000 || task.AllDay {
		t.Errorf("срок: %v all_day=%t", task.Due, task.AllDay)
	}
	if task.Start == nil || *task.Start != 1794562200000 {
		t.Errorf("DTSTART должен идти за сроком: %v", task.Start)
	}
	if task.Priority == nil || *task.Priority != 5 {
		t.Errorf("приоритет не трогали, а он %v", task.Priority)
	}

	row := store.event(id)
	for _, want := range []string{"SUMMARY:Оплата офиса и склада", "DUE:20261113T093000Z", "DTSTART:20261113T093000Z", "X-APPLE-SORT-ORDER:12", "DESCRIPTION:старая"} {
		if !strings.Contains(row.ICalData, want) {
			t.Errorf("в теле нет %q:\n%s", want, row.ICalData)
		}
	}
	if !row.LocalModified {
		t.Error("local_modified не выставлен — воркер не отправит правку")
	}
	q := store.queue[id]
	if q.op != "update" || q.remoteID != "/123/calendars/tasks/FCC55C22.ics" || q.ical != row.ICalData {
		t.Errorf("очередь: %+v", q)
	}
}

func TestTaskPatchClearsDueAndPriority(t *testing.T) {
	store := taskFixtures()
	id := seedAppleTask(store)
	rec := taskCall(t, store, "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", id), `{"due":0,"priority":0,"description":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	task := decodeTask(t, rec)
	if task.Due != nil || task.Start != nil || task.Priority != nil || task.Description != "" || task.AllDay {
		t.Errorf("не снято: %+v", task)
	}
	body := store.event(id).ICalData
	for _, gone := range []string{"DUE", "DTSTART", "PRIORITY", "DESCRIPTION"} {
		if strings.Contains(body, "\n"+gone) {
			t.Errorf("%s осталось в теле:\n%s", gone, body)
		}
	}
}

func TestTaskPatchAllDayOnlyKeepsDate(t *testing.T) {
	store := taskFixtures()
	id := seedAppleTask(store)
	rec := taskCall(t, store, "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", id), `{"all_day":true}`)
	task := decodeTask(t, rec)
	if task.Due == nil || *task.Due != 1789776000000 || !task.AllDay {
		t.Errorf("срок датой: %v all_day=%t", task.Due, task.AllDay)
	}
	if !strings.Contains(store.event(id).ICalData, "DUE;VALUE=DATE:20260919") {
		t.Errorf("тело:\n%s", store.event(id).ICalData)
	}
}

func TestTaskPatchRejects(t *testing.T) {
	store := taskFixtures()
	id := seedAppleTask(store)
	// Задача в чужой коллекции и задача в подписке.
	foreign := &models.CalendarEvent{CalendarID: 4, UID: "f", Component: models.ComponentTodo, Summary: "чужая"}
	ro := &models.CalendarEvent{CalendarID: 3, UID: "r", Component: models.ComponentTodo, Summary: "только чтение"}
	event := &models.CalendarEvent{CalendarID: 2, UID: "e", Component: models.ComponentEvent, Summary: "событие"}
	for _, e := range []*models.CalendarEvent{foreign, ro, event} {
		if err := store.CreateCalendarEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, method, path, body string
		code                     int
	}{
		{"пустой заголовок", "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", id), `{"summary":" "}`, http.StatusBadRequest},
		{"приоритет 10", "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", id), `{"priority":10}`, http.StatusBadRequest},
		{"кривой id", "PATCH", "/api/desktop/v1/tasks/abc", `{}`, http.StatusBadRequest},
		{"нет задачи", "PATCH", "/api/desktop/v1/tasks/9999", `{"summary":"x"}`, http.StatusNotFound},
		{"чужая — как нет", "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", foreign.ID), `{"summary":"x"}`, http.StatusNotFound},
		{"только чтение", "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", ro.ID), `{"summary":"x"}`, http.StatusForbidden},
		{"событие — не задача", "PATCH", fmt.Sprintf("/api/desktop/v1/tasks/%d", event.ID), `{"summary":"x"}`, http.StatusBadRequest},
		{"удалить чужую", "DELETE", fmt.Sprintf("/api/desktop/v1/tasks/%d", foreign.ID), ``, http.StatusNotFound},
		{"удалить из подписки", "DELETE", fmt.Sprintf("/api/desktop/v1/tasks/%d", ro.ID), ``, http.StatusForbidden},
		{"удалить событие", "DELETE", fmt.Sprintf("/api/desktop/v1/tasks/%d", event.ID), ``, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := taskCall(t, store, c.method, c.path, c.body)
			if rec.Code != c.code {
				t.Errorf("код %d, ждали %d: %s", rec.Code, c.code, rec.Body)
			}
		})
	}
	if len(store.queue) != 0 {
		t.Errorf("отказы попали в очередь: %v", store.queue)
	}
	if store.event(foreign.ID) == nil || store.event(ro.ID) == nil || store.event(event.ID) == nil {
		t.Error("отказ в удалении всё-таки удалил")
	}
}

func TestTaskDeleteQueuesDeleteWithRemoteID(t *testing.T) {
	store := taskFixtures()
	id := seedAppleTask(store)
	rec := taskCall(t, store, "DELETE", fmt.Sprintf("/api/desktop/v1/tasks/%d", id), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	if store.event(id) != nil {
		t.Error("строка не удалена")
	}
	q := store.queue[id]
	if q.op != "delete" || q.remoteID != "/123/calendars/tasks/FCC55C22.ics" {
		t.Errorf("очередь: %+v — без remote_id наверху задача останется", q)
	}
}

// Создали и сразу удалили, воркер ещё не успел: в очереди остаётся delete без
// remote_id — воркер такую запись выбрасывает, наверх ничего не уходит.
func TestTaskCreateThenDeleteBeforePush(t *testing.T) {
	store := taskFixtures()
	task := decodeTask(t, taskCall(t, store, "POST", "/api/desktop/v1/tasks", `{"calendar_id":1,"summary":"x"}`))
	if rec := taskCall(t, store, "DELETE", fmt.Sprintf("/api/desktop/v1/tasks/%d", task.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("код %d", rec.Code)
	}
	if q := store.queue[task.ID]; q.op != "delete" || q.remoteID != "" {
		t.Errorf("очередь: %+v", q)
	}
}

func TestTaskCompletionReadOnlyIsForbidden(t *testing.T) {
	store := taskFixtures()
	ro := &models.CalendarEvent{CalendarID: 3, UID: "r", Component: models.ComponentTodo, Summary: "x"}
	if err := store.CreateCalendarEvent(ro); err != nil {
		t.Fatal(err)
	}
	rec := taskCall(t, store, "POST", fmt.Sprintf("/api/desktop/v1/tasks/%d/completion", ro.ID), `{"completed":true}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("код %d", rec.Code)
	}

	id := seedAppleTask(store)
	rec = taskCall(t, store, "POST", fmt.Sprintf("/api/desktop/v1/tasks/%d/completion", id), `{"completed":true}`)
	if rec.Code != http.StatusOK || !store.event(id).IsCompleted() || store.queue[id].op != "update" {
		t.Errorf("отметка: %d %s, очередь %+v", rec.Code, rec.Body, store.queue[id])
	}
}

func TestTaskWriteRequiresUser(t *testing.T) {
	a := taskAPI{store: taskFixtures()}
	for _, h := range []http.HandlerFunc{a.create, a.patch, a.remove} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", "/api/desktop/v1/tasks", bytes.NewReader([]byte(`{}`))))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("без пользователя: %d", rec.Code)
		}
	}
}
