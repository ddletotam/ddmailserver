package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	caldavutil "github.com/ddletotam/ddmailserver/internal/caldav"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/gorilla/mux"
)

// taskStore — то, что обработчикам задач нужно от базы. *db.DB его
// реализует; тесты подставляют хранилище в памяти.
type taskStore interface {
	GetEnabledCalendarsByUserID(userID int64) ([]*models.Calendar, error)
	GetCalendarByID(id int64) (*models.Calendar, error)
	GetTodosForCalendars(calendarIDs []int64, includeCompleted bool) ([]*models.CalendarEvent, error)
	GetEventByID(id int64) (*models.CalendarEvent, error)
	CreateCalendarEvent(event *models.CalendarEvent) error
	UpdateCalendarEvent(event *models.CalendarEvent) error
	DeleteCalendarEvent(id int64) error
	QueueCalendarEventSync(eventID, calendarID, sourceID int64, uid, remoteID, icalData, operation string) error
}

// taskAPI — обработчики /tasks. Отдельный тип, а не методы Server, чтобы
// тесты гоняли их на httptest без PostgreSQL.
type taskAPI struct {
	store taskStore
}

func (s *Server) taskAPI() taskAPI {
	return taskAPI{store: s.database}
}

func userFromContext(ctx context.Context) *models.User {
	user, ok := ctx.Value(userContextKey).(*models.User)
	if !ok {
		return nil
	}
	return user
}

// taskWritable — можно ли менять задачи этой коллекции. ICS-подписки и
// импорт только для чтения, даже если флаг can_write у них почему-то стоит.
func taskWritable(cal *models.Calendar) bool {
	return cal.CanWrite && cal.SourceType != "ics_url" && cal.SourceType != "ics_import"
}

// queueSync ставит правку в очередь обратной синхронизации — тот же путь, что
// у событий (queueEventReverseSync): для CalDAV-коллекций с обратной
// синхронизацией воркер CalendarEventSyncTask сделает PUT/DELETE наверх
// (iCloud, Яндекс и т.п.). Локальные коллекции наш CalDAV-сервер отдаёт
// клиентам сам, им очередь не нужна.
func (a taskAPI) queueSync(cal *models.Calendar, task *models.CalendarEvent, op string) {
	if cal.SourceType != "caldav" || !cal.Enabled || !cal.ReverseSync {
		return
	}
	if err := a.store.QueueCalendarEventSync(task.ID, cal.ID, cal.SourceID, task.UID, task.RemoteID, task.ICalData, op); err != nil {
		log.Printf("task %s: failed to queue reverse sync (%s): %v", task.UID, op, err)
	}
}

// ownedTask загружает задачу пользователя и её коллекцию. Чужая задача
// отвечает 404, как несуществующая: перебор id не должен ничего сообщать.
// При false ответ уже записан.
func (a taskAPI) ownedTask(w http.ResponseWriter, user *models.User, id int64) (*models.CalendarEvent, *models.Calendar, bool) {
	task, err := a.store.GetEventByID(id)
	if err != nil || task == nil {
		respondError(w, http.StatusNotFound, "task not found")
		return nil, nil, false
	}
	cal, err := a.store.GetCalendarByID(task.CalendarID)
	if err != nil || cal == nil || cal.UserID != user.ID {
		respondError(w, http.StatusNotFound, "task not found")
		return nil, nil, false
	}
	if !task.IsTodo() {
		respondError(w, http.StatusBadRequest, "not a task")
		return nil, nil, false
	}
	return task, cal, true
}

// TaskCreateRequest — тело POST /api/desktop/v1/tasks.
//
// Коллекцию выбирает клиент (`calendar_id`), а не сервер по адресу: задачи
// живут в отдельных коллекциях (списки Напоминаний Apple принимают только
// VTODO), и «календарь по умолчанию» для событий для них не годится.
type TaskCreateRequest struct {
	CalendarID  int64  `json:"calendar_id"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	// Due — срок в мс; нет или 0 — без срока. При AllDay — полночь UTC
	// нужной даты (сервер всё равно обрежет до даты).
	Due    *int64 `json:"due"`
	AllDay bool   `json:"all_day"`
	// Priority — 1 (высший) … 9 (низший), 0 — не указан (RFC 5545 §3.8.1.9).
	Priority *int16 `json:"priority"`
}

// TaskPatchRequest — тело PATCH /api/desktop/v1/tasks/{id}. Поле, которого
// нет, не меняется. `due: 0` снимает срок, `priority: 0` — приоритет.
// Перенос в другую коллекцию не поддерживается: наверху это удаление в одном
// источнике и создание в другом, а не правка.
type TaskPatchRequest struct {
	Summary     *string `json:"summary"`
	Description *string `json:"description"`
	Due         *int64  `json:"due"`
	AllDay      *bool   `json:"all_day"`
	Priority    *int16  `json:"priority"`
}

func validPriority(p *int16) bool {
	return p == nil || (*p >= 0 && *p <= 9)
}

// HandleDesktopTaskCreate создаёт задачу в коллекции пользователя, которая
// принимает VTODO и куда можно писать, и ставит её в очередь обратной
// синхронизации. Отвечает 201 и задачей в том же виде, что GET /tasks.
func (s *Server) HandleDesktopTaskCreate(w http.ResponseWriter, r *http.Request) {
	s.taskAPI().create(w, r)
}

func (a taskAPI) create(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req TaskCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Summary = strings.TrimSpace(req.Summary)
	if req.Summary == "" {
		respondError(w, http.StatusBadRequest, "summary is required")
		return
	}
	if req.CalendarID == 0 {
		respondError(w, http.StatusBadRequest, "calendar_id is required")
		return
	}
	if !validPriority(req.Priority) {
		respondError(w, http.StatusBadRequest, "priority must be 0..9")
		return
	}

	cal, err := a.store.GetCalendarByID(req.CalendarID)
	// Выключенная коллекция скрыта от клиента, а её очередь воркер списывает
	// не отправляя — задача пропала бы молча.
	if err != nil || cal == nil || cal.UserID != user.ID || !cal.Enabled {
		respondError(w, http.StatusNotFound, "calendar not found")
		return
	}
	if !taskWritable(cal) {
		respondError(w, http.StatusForbidden, "calendar is read-only")
		return
	}
	// Задачу в коллекцию событий наверх не положить: iCloud отвечает 403 на
	// каждый PUT (см. migrations/048). Отказ здесь, а не в очереди.
	if !cal.Supports(models.ComponentTodo) {
		respondError(w, http.StatusUnprocessableEntity, "calendar does not accept tasks")
		return
	}

	fields := caldavutil.TodoFields{
		Summary:     req.Summary,
		Description: req.Description,
		AllDay:      req.AllDay,
		Priority:    req.Priority,
	}
	if req.Due != nil && *req.Due != 0 {
		due := *req.Due
		if req.AllDay {
			due = caldavutil.TruncateToUTCDate(due)
		}
		fields.Due = &due
	} else {
		fields.AllDay = false
	}
	if fields.Priority != nil && *fields.Priority == 0 {
		fields.Priority = nil
	}

	now := timeutil.Now()
	task := &models.CalendarEvent{
		CalendarID:    cal.ID,
		UID:           generateUID(),
		Component:     models.ComponentTodo,
		Summary:       fields.Summary,
		Description:   fields.Description,
		Due:           fields.Due,
		AllDay:        fields.AllDay,
		Priority:      fields.Priority,
		Status:        "NEEDS-ACTION",
		LocalModified: true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	task.ICalData = caldavutil.BuildTodo(task.UID, fields, now)

	if err := a.store.CreateCalendarEvent(task); err != nil {
		log.Printf("task create: %v", err)
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}

	a.queueSync(cal, task, "create")
	log.Printf("task create: task %d (%s) in calendar %d", task.ID, task.UID, cal.ID)

	respondJSON(w, http.StatusCreated, desktopTaskFrom(task, cal))
}

// HandleDesktopTaskPatch правит заголовок, заметку, срок и приоритет задачи.
// Тело задачи правится по строкам (caldav.SetTodoFields), так что свойства
// Apple, которых мы не моделируем, переживают правку. Отвечает задачей в
// том же виде, что GET /tasks.
func (s *Server) HandleDesktopTaskPatch(w http.ResponseWriter, r *http.Request) {
	s.taskAPI().patch(w, r)
}

func (a taskAPI) patch(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	taskID, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad task id")
		return
	}

	var req TaskPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Summary != nil {
		trimmed := strings.TrimSpace(*req.Summary)
		if trimmed == "" {
			respondError(w, http.StatusBadRequest, "summary must not be empty")
			return
		}
		req.Summary = &trimmed
	}
	if !validPriority(req.Priority) {
		respondError(w, http.StatusBadRequest, "priority must be 0..9")
		return
	}

	task, cal, ok := a.ownedTask(w, user, taskID)
	if !ok {
		return
	}
	if !taskWritable(cal) {
		respondError(w, http.StatusForbidden, "calendar is read-only")
		return
	}

	patch := caldavutil.TodoPatch{
		Summary:     req.Summary,
		Description: req.Description,
		Priority:    req.Priority,
	}
	switch {
	case req.Due != nil:
		patch.SetDue = true
		if *req.Due != 0 {
			due := *req.Due
			// Без all_day срок остаётся того же вида, что был.
			patch.AllDay = task.AllDay
			if req.AllDay != nil {
				patch.AllDay = *req.AllDay
			}
			if patch.AllDay {
				due = caldavutil.TruncateToUTCDate(due)
			}
			patch.Due = &due
		}
	case req.AllDay != nil && task.Due != nil:
		// Срок тот же, меняется только «датой или со временем».
		patch.SetDue = true
		due := *task.Due
		patch.AllDay = *req.AllDay
		if patch.AllDay {
			due = caldavutil.TruncateToUTCDate(due)
		}
		patch.Due = &due
	}

	now := timeutil.Now()
	task.ICalData = caldavutil.SetTodoFields(task.ICalData, patch, now)

	// Индексированные поля — по тем же правилам, что и тело.
	if patch.Summary != nil {
		task.Summary = *patch.Summary
	}
	if patch.Description != nil {
		task.Description = *patch.Description
	}
	if patch.Priority != nil {
		if *patch.Priority == 0 {
			task.Priority = nil
		} else {
			p := *patch.Priority
			task.Priority = &p
		}
	}
	if patch.SetDue {
		hadStart := task.DTStart != 0
		task.Due = patch.Due
		task.AllDay = patch.Due != nil && patch.AllDay
		task.DTStart = 0
		if hadStart && patch.Due != nil {
			task.DTStart = *patch.Due
		}
	}
	task.UpdatedAt = now
	task.LocalModified = true

	if err := a.store.UpdateCalendarEvent(task); err != nil {
		log.Printf("task patch: failed to update task %d: %v", taskID, err)
		respondError(w, http.StatusInternalServerError, "failed to update task")
		return
	}
	a.queueSync(cal, task, "update")

	respondJSON(w, http.StatusOK, desktopTaskFrom(task, cal))
}

// HandleDesktopTaskDelete удаляет задачу; во внешнем CalDAV-источнике её
// удалит воркер обратной синхронизации.
func (s *Server) HandleDesktopTaskDelete(w http.ResponseWriter, r *http.Request) {
	s.taskAPI().remove(w, r)
}

func (a taskAPI) remove(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	taskID, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad task id")
		return
	}

	task, cal, ok := a.ownedTask(w, user, taskID)
	if !ok {
		return
	}
	if !taskWritable(cal) {
		respondError(w, http.StatusForbidden, "calendar is read-only")
		return
	}

	// В очередь — ДО удаления строки: только живая строка знает remote_id, без
	// него наверху задача осталась бы навсегда. Задача, которую ещё не успели
	// отправить, заменяет свой "create" на "delete" без remote_id — воркер
	// такую просто выбрасывает.
	a.queueSync(cal, task, "delete")

	if err := a.store.DeleteCalendarEvent(task.ID); err != nil {
		log.Printf("task delete: %v", err)
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
