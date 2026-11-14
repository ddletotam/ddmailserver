package web

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/push"
)

// Регистрация устройств для push (ios-behavior-contract.md §7,
// android-behavior-contract.md §7): одна на все платформы, служба — kind
// (apns, hms, rustore; fcm — зарезервирован).
//
//	POST   /api/desktop/v1/push/token  {"kind","token","environment","topic"}
//	DELETE /api/desktop/v1/push/token  {"kind","token"} (или ?kind=&token=)
//
// Регистрация идемпотентна: клиент шлёт её при каждом запуске и при смене
// токена. Выход из учётки на устройстве — DELETE.

// maxPushTokenBody — тело запроса; токены FCM бывают под 200 символов,
// HMS и RuStore — длиннее, но не килобайты.
const maxPushTokenBody = 8 << 10

// PushTokenStore — где сервер хранит токены. *db.DB его реализует; тесты —
// подмена.
type PushTokenStore interface {
	UpsertPushToken(ctx context.Context, t models.PushToken) error
	DeleteUserPushToken(ctx context.Context, userID int64, kind, token string) (bool, error)
}

type pushTokenRequest struct {
	Kind        string `json:"kind"`
	Token       string `json:"token"`
	Environment string `json:"environment"`
	Topic       string `json:"topic"`
}

// pushTokenResponse: delivery — умеет ли сервер сейчас доставлять через эту
// службу (настроен ли ключ). Регистрация принимается и без этого: ключ
// появится — push пойдёт без повторной регистрации.
type pushTokenResponse struct {
	Kind     string `json:"kind"`
	Delivery bool   `json:"delivery"`
}

func (s *Server) pushTokenStore() PushTokenStore {
	if s.pushTokens != nil {
		return s.pushTokens
	}
	return s.database
}

// HandleDesktopPushTokenRegister — POST /push/token.
func (s *Server) HandleDesktopPushTokenRegister(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	req, ok := readPushTokenRequest(w, r)
	if !ok {
		return
	}
	t, msg := normalizePushToken(req, true)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	t.UserID = user.ID
	if err := s.pushTokenStore().UpsertPushToken(r.Context(), t); err != nil {
		log.Printf("push token: register user=%d: %v", user.ID, err)
		respondError(w, http.StatusInternalServerError, "failed to register push token")
		return
	}
	respondJSON(w, http.StatusOK, pushTokenResponse{Kind: t.Kind, Delivery: s.push.Delivers(t.Kind)})
}

// HandleDesktopPushTokenDelete — DELETE /push/token. Нет такого токена —
// тоже успех: выход повторяют, пока не дойдёт.
func (s *Server) HandleDesktopPushTokenDelete(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req pushTokenRequest
	if q := r.URL.Query(); q.Get("token") != "" {
		req.Kind, req.Token = q.Get("kind"), q.Get("token")
	} else {
		var ok bool
		if req, ok = readPushTokenRequest(w, r); !ok {
			return
		}
	}
	t, msg := normalizePushToken(req, false)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	deleted, err := s.pushTokenStore().DeleteUserPushToken(r.Context(), user.ID, t.Kind, t.Token)
	if err != nil {
		log.Printf("push token: delete user=%d: %v", user.ID, err)
		respondError(w, http.StatusInternalServerError, "failed to delete push token")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": deleted})
}

func readPushTokenRequest(w http.ResponseWriter, r *http.Request) (pushTokenRequest, bool) {
	var req pushTokenRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPushTokenBody+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to read body")
		return req, false
	}
	if len(body) > maxPushTokenBody {
		respondError(w, http.StatusRequestEntityTooLarge, "body too large")
		return req, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON")
		return req, false
	}
	return req, true
}

// normalizePushToken проверяет запрос и приводит токен к хранимому виду.
// full — регистрация (окружение и topic тоже); иначе нужны только служба и
// токен. Ответ с непустым текстом — ошибка для клиента.
func normalizePushToken(req pushTokenRequest, full bool) (models.PushToken, string) {
	t := models.PushToken{
		Kind:        strings.ToLower(strings.TrimSpace(req.Kind)),
		Token:       strings.TrimSpace(req.Token),
		Environment: strings.ToLower(strings.TrimSpace(req.Environment)),
		Topic:       strings.TrimSpace(req.Topic),
	}
	switch t.Kind {
	case models.PushKindAPNs:
		// Токен APNs — байты устройства в hex (сейчас 32 байта; Apple просит
		// не полагаться на длину). Регистр не важен — храним в нижнем.
		t.Token = strings.ToLower(t.Token)
		if len(t.Token) < 16 || len(t.Token) > 512 || len(t.Token)%2 != 0 || !onlyChars(t.Token, "0123456789abcdef") {
			return t, "invalid apns token"
		}
		if full && t.Environment != models.PushEnvSandbox && t.Environment != models.PushEnvProduction {
			return t, "environment must be sandbox or production"
		}
	case models.PushKindHMS, models.PushKindRuStore, models.PushKindFCM:
		if t.Token == "" || len(t.Token) > 4096 || !onlyChars(t.Token, tokenChars) {
			return t, "invalid " + t.Kind + " token"
		}
		if full && t.Environment != "" {
			return t, "environment is only for apns"
		}
	default:
		return t, "kind must be apns, hms, rustore or fcm"
	}
	if !full {
		t.Environment, t.Topic = "", ""
		return t, ""
	}
	if len(t.Topic) > 255 || !onlyChars(t.Topic, topicChars) {
		return t, "invalid topic"
	}
	return t, ""
}

// Символы токенов HMS/RuStore/FCM (base64url, двоеточие FCM) и topic (bundle id,
// имя пакета Android).
const (
	tokenChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_:.+/="
	topicChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_."
)

func onlyChars(s, allowed string) bool {
	for _, c := range s {
		if !strings.ContainsRune(allowed, c) {
			return false
		}
	}
	return true
}

// SetPush — диспетчер push: по нему регистрация отвечает, доставляет ли
// сервер через службу. nil — не доставляет ни через какую.
func (s *Server) SetPush(d *push.Dispatcher) {
	s.push = d
}
