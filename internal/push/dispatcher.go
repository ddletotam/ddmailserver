package push

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/ddletotam/ddmailserver/internal/notify"
)

// Значения Dispatcher по умолчанию.
const (
	// DefaultWindow — не больше одного push пользователю за это время. Письма
	// приходят пачками (синк внешнего ящика, рассылка на несколько адресов):
	// первое будит устройство сразу, остальные окна сливаются в один push в
	// его конце. Клиент всё равно синхронизирует всё разом.
	DefaultWindow = 10 * time.Second
	// sendTimeout — одна доставка на одно устройство.
	sendTimeout = 15 * time.Second
	// maxParallel — сколько пользователей обслуживается одновременно.
	maxParallel = 8
	// pruneAbove — после стольких пользователей в памяти забываются те, у
	// кого окно давно закрылось.
	pruneAbove = 256
)

// Dispatcher превращает события о новой почте в push на устройства
// пользователя, не чаще раза в окно на пользователя.
type Dispatcher struct {
	store   Store
	senders map[string]Sender
	window  time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	sem    chan struct{}

	mu     sync.Mutex
	users  map[int64]*userWindow
	closed bool
}

// userWindow — когда пользователю ушёл последний push и ждёт ли его push в
// конце окна.
type userWindow struct {
	last  time.Time
	timer *time.Timer
}

// NewDispatcher — window 0 означает DefaultWindow. Отправители с
// одинаковой службой: последний заменяет прежних.
func NewDispatcher(store Store, window time.Duration, senders ...Sender) *Dispatcher {
	if window <= 0 {
		window = DefaultWindow
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		store:   store,
		senders: make(map[string]Sender),
		window:  window,
		ctx:     ctx,
		cancel:  cancel,
		sem:     make(chan struct{}, maxParallel),
		users:   make(map[int64]*userWindow),
	}
	for _, s := range senders {
		if s != nil {
			d.senders[s.Kind()] = s
		}
	}
	return d
}

// Kinds — службы, через которые сервер умеет доставлять (по алфавиту).
func (d *Dispatcher) Kinds() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.senders))
	for k := range d.senders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Delivers — есть ли отправитель у службы.
func (d *Dispatcher) Delivers(kind string) bool {
	if d == nil {
		return false
	}
	_, ok := d.senders[kind]
	return ok
}

// Attach подписывает диспетчер на все события хаба. Остановка — Stop.
func (d *Dispatcher) Attach(hub *notify.Hub) {
	ch := hub.SubscribeAll()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if IsNewMail(ev) {
					d.NewMail(ev.UserID)
				}
			case <-d.ctx.Done():
				hub.UnsubscribeAll(ch)
				return
			}
		}
	}()
}

// NewMail — пользователю пришло новое письмо. Окно свободно — push уходит
// сразу; занято — один push в конце окна, сколько бы писем ни пришло.
func (d *Dispatcher) NewMail(userID int64) {
	if len(d.senders) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	w := d.users[userID]
	if w == nil {
		d.prune()
		w = &userWindow{}
		d.users[userID] = w
	}
	if w.timer != nil {
		return // push в конце окна уже ждёт
	}
	now := time.Now()
	wait := d.window - now.Sub(w.last)
	if w.last.IsZero() || wait <= 0 {
		w.last = now
		d.spawn(userID)
		return
	}
	w.timer = time.AfterFunc(wait, func() { d.flush(userID) })
}

// flush — конец окна: отложенный push уходит.
func (d *Dispatcher) flush(userID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.users[userID]
	if d.closed || w == nil {
		return
	}
	w.timer = nil
	w.last = time.Now()
	d.spawn(userID)
}

// prune забывает пользователей с давно закрытым окном. Вызывается под mu.
func (d *Dispatcher) prune() {
	if len(d.users) < pruneAbove {
		return
	}
	cutoff := time.Now().Add(-d.window)
	for id, w := range d.users {
		if w.timer == nil && w.last.Before(cutoff) {
			delete(d.users, id)
		}
	}
}

// spawn — доставка пользователю в своей горутине. Вызывается под mu.
func (d *Dispatcher) spawn(userID int64) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		select {
		case d.sem <- struct{}{}:
		case <-d.ctx.Done():
			return
		}
		defer func() { <-d.sem }()
		d.deliver(userID)
	}()
}

// deliver — push на все устройства пользователя.
func (d *Dispatcher) deliver(userID int64) {
	tokens, err := d.store.PushTokensForUser(d.ctx, userID)
	if err != nil {
		log.Printf("Push: tokens of user %d: %v", userID, err)
		return
	}
	for _, t := range tokens {
		s := d.senders[t.Kind]
		if s == nil || !s.Accepts(t) {
			continue
		}
		ctx, cancel := context.WithTimeout(d.ctx, sendTimeout)
		err := s.Send(ctx, t)
		cancel()
		var rejected *TokenRejectedError
		switch {
		case err == nil:
			if err := d.store.MarkPushTokenSent(d.ctx, t.ID); err != nil {
				log.Printf("Push: %v", err)
			}
		case errors.As(err, &rejected):
			log.Printf("Push: %s token %s of user %d rejected (%d %s) — removing",
				t.Kind, maskToken(t.Token), userID, rejected.Status, rejected.Reason)
			if err := d.store.DeletePushToken(d.ctx, t.Kind, t.Token); err != nil {
				log.Printf("Push: %v", err)
			}
		default:
			log.Printf("Push: %s to user %d (token %s) failed: %v", t.Kind, userID, maskToken(t.Token), err)
		}
	}
}

// Stop отписывается от хаба, отменяет отложенные push и обрывает идущие
// доставки. Push — не почта: потерять «есть новые письма» при остановке
// сервера не страшно, клиент синхронизируется при следующем запуске.
func (d *Dispatcher) Stop() error {
	d.mu.Lock()
	d.closed = true
	for _, w := range d.users {
		if w.timer != nil {
			w.timer.Stop()
			w.timer = nil
		}
	}
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
	return nil
}
