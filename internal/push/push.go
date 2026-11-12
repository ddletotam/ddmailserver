// Package push доставляет клиентам в фоне весть «есть новые письма» через
// службы push: APNs (iPhone, iPad, Mac), в будущем HMS и FCM (Android).
//
// Что уходит в push — только факт: службы доставки — серверы за рубежом, и
// отправитель, тема или адреса там были бы передачей ПДн третьему лицу
// (152-ФЗ). Текст уведомления клиент собирает сам после синхронизации, из
// своего кэша (ios-behavior-contract.md §7, android-behavior-contract.md §7).
//
// Поток: notify.Hub → Dispatcher (отбор «настоящего нового входящего»,
// коалесинг по пользователю) → Sender своей службы для каждого токена
// пользователя. Токен, который служба назвала недействительным, удаляется.
package push

import (
	"context"
	"fmt"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
)

// Sender — доставка через одну службу (APNs, HMS, FCM).
type Sender interface {
	// Kind — служба: models.PushKindAPNs и т.д. Совпадает с push_tokens.kind.
	Kind() string
	// Accepts — обслуживает ли отправитель этот токен (окружение, topic).
	// Чужой токен пропускается молча: он не недействителен, он не наш.
	Accepts(t models.PushToken) bool
	// Send — один push «есть новые письма» на устройство. Ошибка
	// *TokenRejectedError — токена больше нет, его надо удалить.
	Send(ctx context.Context, t models.PushToken) error
}

// Store — где лежат токены. *db.DB его реализует; тесты — подмена.
type Store interface {
	PushTokensForUser(ctx context.Context, userID int64) ([]models.PushToken, error)
	DeletePushToken(ctx context.Context, kind, token string) error
	MarkPushTokenSent(ctx context.Context, id int64) error
}

// TokenRejectedError — служба доставки ответила, что устройства с этим
// токеном больше нет (приложение удалено, токен сменился, токен чужого
// окружения). Такой токен удаляется, повтор бесполезен.
type TokenRejectedError struct {
	Status int
	Reason string
}

func (e *TokenRejectedError) Error() string {
	return fmt.Sprintf("push token rejected: %d %s", e.Status, e.Reason)
}

// IsNewMail отбирает события, о которых стоит будить устройство: настоящее
// новое входящее письмо.
//
// new_message публикуют три пути — локальная доставка (только не спам),
// синк внешних ящиков (только если среди новых есть не спам) и письмо-
// предупреждение о сбое календаря; у всех есть id письма. Возврат писем из
// спама руками («Не спам») тоже публикует new_message, но без id письма —
// пользователь сам нажал кнопку, будить ради этого его телефон незачем.
func IsNewMail(e notify.Event) bool {
	if e.Type != notify.EventNewMessage || e.UserID == 0 || e.MessageID == 0 {
		return false
	}
	return e.Mailbox == "" || strings.EqualFold(e.Mailbox, "INBOX")
}

// maskToken — начало токена для журнала: токен — адрес устройства, целиком
// ему в журнале делать нечего.
func maskToken(t string) string {
	if len(t) <= 8 {
		return t
	}
	return t[:8] + "…"
}
