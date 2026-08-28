package client

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	imapClient "github.com/emersion/go-imap/client"
)

// fakeIMAP поднимает минимальный IMAP-сервер, который отвечает заданным набором
// возможностей и НЕ умеет STARTTLS. Достаточно для проверки того единственного
// решения, которое нас здесь интересует: отдавать ли такому серверу пароль.
func fakeIMAP(t *testing.T, capabilities string) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		if _, err := conn.Write([]byte("* OK [CAPABILITY " + capabilities + "] fake ready\r\n")); err != nil {
			return
		}
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			tag, cmd := fields[0], strings.ToUpper(fields[1])
			switch cmd {
			case "CAPABILITY":
				_, _ = conn.Write([]byte("* CAPABILITY " + capabilities + "\r\n" + tag + " OK done\r\n"))
			case "LOGOUT":
				_, _ = conn.Write([]byte("* BYE\r\n" + tag + " OK done\r\n"))
				return
			default:
				_, _ = conn.Write([]byte(tag + " BAD unexpected\r\n"))
			}
		}
	}()

	return ln.Addr()
}

// TestUpgradeStartTLSRefusesServerWithoutSTARTTLS: ветка «порт без неявного TLS»
// раньше означала буквально открытый текст — следом шёл LOGIN с паролем.
// Сервер, не предлагающий STARTTLS, должен получить отказ, а не учётные данные.
func TestUpgradeStartTLSRefusesServerWithoutSTARTTLS(t *testing.T) {
	addr := fakeIMAP(t, "IMAP4rev1 LOGIN")

	conn, err := imapClient.Dial(addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Logout()

	err = upgradeStartTLS(conn, "fake.invalid")
	if err == nil {
		t.Fatal("сервер без STARTTLS принят — пароль ушёл бы открытым текстом")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("ошибка не называет причину: %v", err)
	}
}

// А вот сервер, который STARTTLS предлагает, до отказа доходить не должен:
// проверка возможностей обязана его пропустить, и упасть он может только уже
// на самом рукопожатии (настоящего TLS у болванки нет).
func TestUpgradeStartTLSProceedsWhenOffered(t *testing.T) {
	addr := fakeIMAP(t, "IMAP4rev1 STARTTLS LOGINDISABLED")

	conn, err := imapClient.Dial(addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Logout()

	err = upgradeStartTLS(conn, "fake.invalid")
	if err == nil {
		t.Skip("болванка не умеет настоящий TLS — до успеха дойти не может")
	}
	if strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("сервер предлагал STARTTLS, но получил отказ по возможностям: %v", err)
	}
}
