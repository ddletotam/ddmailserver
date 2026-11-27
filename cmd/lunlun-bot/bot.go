package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"
	"sync"
	"time"
)

// messenger is the part of Telegram the bot uses (fake in tests).
type messenger interface {
	getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error)
	send(ctx context.Context, chatID int64, text string, kb *keyboard) (int64, error)
	deleteMessage(ctx context.Context, chatID, messageID int64) error
	dropKeyboard(ctx context.Context, chatID, messageID int64) error
	answerCallback(ctx context.Context, id string) error
	setCommands(ctx context.Context, cmds []command) error
}

// accounts is the mail server's bot API (fake in tests).
type accounts interface {
	getAccount(ctx context.Context, tg int64) (*account, error)
	checkLogin(ctx context.Context, login string) (*loginCheck, error)
	createAccount(ctx context.Context, tg int64, login, lang string) (*account, error)
	resetPassword(ctx context.Context, tg int64) (*account, error)
}

var botCommands = []command{
	{"start", "Мой адрес / регистрация"},
	{"password", "Новый пароль"},
	{"settings", "Настройки почтовых программ"},
	{"help", "Помощь"},
}

// Callback data.
const (
	cbAccept     = "accept"
	cbCreate     = "create:"
	cbOtherLogin = "other"
	cbPassword   = "password"
	cbPasswordOK = "password-ok"
	cbSettings   = "settings"
)

type bot struct {
	cfg config
	tg  messenger
	api accounts

	mu sync.Mutex
	// awaitingLogin: users who accepted the terms and are choosing a login.
	awaitingLogin map[int64]bool
	// lastAction throttles a user to one action per second.
	lastAction map[int64]time.Time
}

func newBot(c config, tg messenger, api accounts) *bot {
	return &bot{cfg: c, tg: tg, api: api, awaitingLogin: map[int64]bool{}, lastAction: map[int64]time.Time{}}
}

func (b *bot) run(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		ups, err := b.tg.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("getUpdates: %v", err)
				sleep(ctx, 5*time.Second)
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			b.handle(ctx, u)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (b *bot) handle(ctx context.Context, u tgUpdate) {
	switch {
	case u.Message != nil:
		m := u.Message
		// Private chats only: a password must never land in a group.
		if m.Chat.Type != "private" || m.From == nil || m.From.IsBot || !b.allow(m.From.ID) {
			return
		}
		b.onText(ctx, m.Chat.ID, *m.From, strings.TrimSpace(m.Text))
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if err := b.tg.answerCallback(ctx, q.ID); err != nil {
			log.Printf("answerCallback: %v", err)
		}
		if q.Message == nil || q.Message.Chat.Type != "private" || !b.allow(q.From.ID) {
			return
		}
		if err := b.tg.dropKeyboard(ctx, q.Message.Chat.ID, q.Message.MessageID); err != nil {
			log.Printf("dropKeyboard: %v", err)
		}
		b.onButton(ctx, q.Message.Chat.ID, q.From, q.Data)
	}
}

func (b *bot) allow(user int64) bool {
	if len(b.cfg.allow) > 0 && !b.cfg.allow[user] {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.lastAction[user]) < time.Second {
		return false
	}
	b.lastAction[user] = now
	return true
}

func (b *bot) setAwaiting(user int64, v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v {
		b.awaitingLogin[user] = true
	} else {
		delete(b.awaitingLogin, user)
	}
}

func (b *bot) awaiting(user int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.awaitingLogin[user]
}

func (b *bot) onText(ctx context.Context, chat int64, from tgUser, text string) {
	switch strings.ToLower(strings.SplitN(text, "@", 2)[0]) {
	case "/start":
		b.setAwaiting(from.ID, false)
		b.start(ctx, chat, from)
		return
	case "/password":
		b.askPassword(ctx, chat, from)
		return
	case "/settings":
		b.say(ctx, chat, b.settingsText(""), nil)
		return
	case "/help":
		b.say(ctx, chat, b.helpText(), nil)
		return
	}
	if b.awaiting(from.ID) {
		b.proposeLogin(ctx, chat, text)
		return
	}
	b.start(ctx, chat, from)
}

func (b *bot) onButton(ctx context.Context, chat int64, from tgUser, data string) {
	switch {
	case data == cbAccept || data == cbOtherLogin:
		b.setAwaiting(from.ID, true)
		b.say(ctx, chat, loginPrompt, nil)
	case strings.HasPrefix(data, cbCreate):
		b.create(ctx, chat, from, strings.TrimPrefix(data, cbCreate))
	case data == cbPassword:
		b.askPassword(ctx, chat, from)
	case data == cbPasswordOK:
		b.resetPassword(ctx, chat, from)
	case data == cbSettings:
		b.say(ctx, chat, b.settingsText(""), nil)
	}
}

// start: the account if there is one, otherwise the terms.
func (b *bot) start(ctx context.Context, chat int64, from tgUser) {
	a, err := b.api.getAccount(ctx, from.ID)
	switch {
	case err == nil:
		b.showAccount(ctx, chat, a)
	case errCode(err) == "not_found":
		b.say(ctx, chat, fmt.Sprintf(
			"Здравствуйте! Это регистрация в почте <b>lunlun.email</b>.\n\n"+
				"Бесплатно — один адрес вида <code>логин@lunlun.email</code> на аккаунт Telegram. "+
				"Войти в почту можно из приложения DDMail или любой почтовой программы.\n\n"+
				"Перед регистрацией прочитайте <a href=\"%s/terms.html\">условия использования</a> "+
				"и <a href=\"%s/privacy.html\">политику конфиденциальности</a>.",
			b.cfg.siteURL, b.cfg.siteURL),
			&keyboard{Rows: [][]button{{{Text: "Принимаю условия", Data: cbAccept}}}})
	case errCode(err) == "banned":
		b.say(ctx, chat, bannedText, nil)
	default:
		b.fail(ctx, chat, err)
	}
}

func (b *bot) showAccount(ctx context.Context, chat int64, a *account) {
	b.say(ctx, chat, fmt.Sprintf("Ваш адрес: <code>%s</code>", html.EscapeString(a.Address)),
		&keyboard{Rows: [][]button{
			{{Text: "Новый пароль", Data: cbPassword}},
			{{Text: "Настройки почтовых программ", Data: cbSettings}},
			{{Text: "Приложение DDMail", URL: b.cfg.siteURL + "/apps.html"}},
		}})
}

const loginPrompt = "Придумайте логин — он станет адресом <code>логин@lunlun.email</code>.\n\n" +
	"3–32 символа: латинские буквы, цифры, точка, дефис, подчёркивание. Начинается с буквы."

const bannedText = "Доступ к регистрации для этого аккаунта закрыт."

func (b *bot) proposeLogin(ctx context.Context, chat int64, text string) {
	c, err := b.api.checkLogin(ctx, text)
	if err != nil {
		b.fail(ctx, chat, err)
		return
	}
	if !c.Available {
		b.say(ctx, chat, reasonText(c.Reason, c.Address)+"\n\nПопробуйте другой логин.", nil)
		return
	}
	b.say(ctx, chat, fmt.Sprintf("Адрес <code>%s</code> свободен.", html.EscapeString(c.Address)),
		&keyboard{Rows: [][]button{
			{{Text: "Создать " + c.Address, Data: cbCreate + c.Login}},
			{{Text: "Другой логин", Data: cbOtherLogin}},
		}})
}

func reasonText(reason, address string) string {
	switch reason {
	case "taken":
		return fmt.Sprintf("Адрес <code>%s</code> уже занят.", html.EscapeString(address))
	case "reserved":
		return "Этот логин зарезервирован: служебные имена и названия организаций не выдаются."
	default:
		return "Так логин не получится: 3–32 символа, латинские буквы, цифры, точка, дефис или подчёркивание; начинается с буквы."
	}
}

func (b *bot) create(ctx context.Context, chat int64, from tgUser, login string) {
	a, err := b.api.createAccount(ctx, from.ID, login, from.LanguageCode)
	switch code := errCode(err); {
	case err == nil:
		b.setAwaiting(from.ID, false)
		b.sendCredentials(ctx, chat, a, "Готово! Почта создана.")
	case code == "registered":
		b.setAwaiting(from.ID, false)
		b.say(ctx, chat, "У вас уже есть адрес — бесплатно выдаётся один на аккаунт Telegram.", nil)
		b.start(ctx, chat, from)
	case code == "taken", code == "reserved", code == "invalid":
		b.setAwaiting(from.ID, true)
		b.say(ctx, chat, reasonText(code, login+"@lunlun.email")+"\n\nПопробуйте другой логин.", nil)
	case code == "banned":
		b.say(ctx, chat, bannedText, nil)
	default:
		b.fail(ctx, chat, err)
	}
}

func (b *bot) askPassword(ctx context.Context, chat int64, from tgUser) {
	b.say(ctx, chat, "Выдать новый пароль? Старый сразу перестанет работать — почтовые программы попросят ввести новый.",
		&keyboard{Rows: [][]button{{{Text: "Да, новый пароль", Data: cbPasswordOK}}}})
}

func (b *bot) resetPassword(ctx context.Context, chat int64, from tgUser) {
	a, err := b.api.resetPassword(ctx, from.ID)
	switch errCode(err) {
	case "":
		if err != nil {
			b.fail(ctx, chat, err)
			return
		}
		b.sendCredentials(ctx, chat, a, "Новый пароль готов.")
	case "not_found":
		b.start(ctx, chat, from)
	case "banned":
		b.say(ctx, chat, bannedText, nil)
	default:
		b.fail(ctx, chat, err)
	}
}

// sendCredentials shows the password once and deletes the message after
// credsTTL, so it does not stay in the chat history.
func (b *bot) sendCredentials(ctx context.Context, chat int64, a *account, title string) {
	text := fmt.Sprintf("%s\n\nАдрес: <code>%s</code>\nПароль: <tg-spoiler><code>%s</code></tg-spoiler>\n\n"+
		"Сохраните пароль: это сообщение удалится через %d минут. Забудете — получите новый здесь же, командой /password.\n\n%s",
		title, html.EscapeString(a.Address), html.EscapeString(a.Password), int(b.cfg.credsTTL.Minutes()), b.settingsText(a.Address))
	id, err := b.tg.send(ctx, chat, text, nil)
	if err != nil {
		log.Printf("send credentials: %v", err)
		return
	}
	go func() {
		time.Sleep(b.cfg.credsTTL)
		if err := b.tg.deleteMessage(context.Background(), chat, id); err != nil {
			log.Printf("delete credentials message: %v", err)
		}
	}()
}

func (b *bot) settingsText(address string) string {
	login := "ваш адрес целиком"
	if address != "" {
		login = "<code>" + html.EscapeString(address) + "</code>"
	}
	h := html.EscapeString(b.cfg.mailHost)
	return fmt.Sprintf("<b>Приложение DDMail</b> — введите адрес и пароль, остальное оно настроит само: %s/apps.html\n\n"+
		"<b>Другие программы</b> (Thunderbird, Outlook, «Почта»):\n"+
		"Входящая (IMAP): <code>%s</code>, порт 993, SSL/TLS\n"+
		"Исходящая (SMTP): <code>%s</code>, порт 465, SSL/TLS\n"+
		"Логин: %s", b.cfg.siteURL, h, h, login)
}

func (b *bot) helpText() string {
	return "/start — ваш адрес или регистрация\n/password — новый пароль\n/settings — настройки почтовых программ\n\n" +
		"Сайт сервиса: " + b.cfg.siteURL
}

func (b *bot) say(ctx context.Context, chat int64, text string, kb *keyboard) {
	if _, err := b.tg.send(ctx, chat, text, kb); err != nil {
		log.Printf("send to %d: %v", chat, err)
	}
}

func (b *bot) fail(ctx context.Context, chat int64, err error) {
	log.Printf("chat %d: %v", chat, err)
	b.say(ctx, chat, "Что-то пошло не так на нашей стороне. Попробуйте через пару минут.", nil)
}
