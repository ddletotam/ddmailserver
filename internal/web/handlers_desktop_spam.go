package web

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/ddletotam/ddmailserver/internal/notify"
)

// spamConvIDPrefix отделяет беседы спама от обычных: один и тот же набор
// адресов может быть и там, и там, а клиент ключует беседы (закрепления,
// папки, кэш) по id.
const spamConvIDPrefix = "spam:"

// spamConversationsMax — потолок писем в выдаче спама. Спам живёт ограниченно
// (worker чистит старый), так что упираться в него не должны.
const spamConversationsMax = 2000

// respondSpamConversations отдаёт беседы из писем-спама пользователя — то, что
// клиенты показывают в системной папке «Спам». Группировка та же, что у
// обычного списка; отличаются источник писем, префикс id и флаг spam.
func (s *Server) respondSpamConversations(w http.ResponseWriter, userID int64, limit int, cc convContext) {
	max := limit
	if max > spamConversationsMax {
		max = spamConversationsMax
	}
	messages, err := s.database.GetSpamMessagesMeta(userID, max)
	if err != nil {
		log.Printf("HandleDesktopConversations(spam): %v", err)
		respondError(w, http.StatusInternalServerError, "spam conversations failed")
		return
	}
	convs := groupConversations(messages, cc)
	for i := range convs {
		convs[i].ID = spamConvIDPrefix + convs[i].ID
		convs[i].Spam = true
	}
	sortConversationsByDate(convs)
	respondJSON(w, http.StatusOK, convs)
}

// HandleDesktopSpamRestore — «Не спам» у беседы в папке «Спам»: каждое
// переданное письмо, которое сейчас спам, возвращается во «Входящие» (как
// «Восстановить» на веб-странице /spam). Правил не создаёт: отправитель,
// пойманный анализатором, может попасть в спам снова — белый список
// настраивается отдельно.
func (s *Server) HandleDesktopSpamRestore(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		Messages []struct {
			UID       int64  `json:"uid"`
			MessageID string `json:"message_id"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	restored := 0
	for _, m := range req.Messages {
		msg := s.resolveSpamRef(user.ID, m.MessageID, m.UID)
		if msg == 0 {
			continue
		}
		if err := s.database.RestoreFromSpam(msg, user.ID); err != nil {
			log.Printf("desktop spam restore: message %d: %v", msg, err)
			continue
		}
		restored++
	}

	log.Printf("desktop spam restore: user=%d restored=%d/%d", user.ID, restored, len(req.Messages))
	if restored > 0 && s.notifyHub != nil {
		// flags_changed, а не new_message: клиенты перечитывают список, но
		// не поднимают тост «новое письмо» о давно пришедшем.
		s.notifyHub.Publish(notify.Event{
			UserID:   user.ID,
			Type:     notify.EventFlagsChanged,
			Username: user.Username,
			Mailbox:  "INBOX",
		})
	}
	respondJSON(w, http.StatusOK, map[string]int{"restored": restored})
}

// resolveSpamRef находит письмо-спам пользователя по ссылке клиента. Uid —
// это id письма в базе (так его отдаёт /conversations), Message-ID — запасной
// ключ. Не спам или чужое — 0.
func (s *Server) resolveSpamRef(userID int64, messageID string, uid int64) int64 {
	if uid > 0 {
		if st, err := s.database.GetMessageSpamState(uid); err == nil && st.IsSpam {
			if msg, err := s.database.GetMessageByID(uid); err == nil && msg != nil && msg.UserID == userID {
				return msg.ID
			}
		}
	}
	if messageID != "" {
		if msg, err := s.database.GetMessageByMessageID(userID, messageID); err == nil && msg != nil && msg.UserID == userID {
			if st, err := s.database.GetMessageSpamState(msg.ID); err == nil && st.IsSpam {
				return msg.ID
			}
		}
	}
	return 0
}
