package client

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// privilegeProbeBody — PROPFIND одного свойства: права текущего пользователя
// на коллекцию (RFC 3744 §5.4).
const privilegeProbeBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:propfind xmlns:D="DAV:"><D:prop><D:current-user-privilege-set/></D:prop></D:propfind>`

// BookWritable спрашивает у сервера, может ли пользователь менять карточки
// коллекции. known=false — сервер свойство не отдал (или не ответил): решать
// по нему нельзя, вызывающий оставляет то, что знал.
//
// Корпоративный каталог SOGo (GAL) приходит в той же «домашней» коллекции,
// что и личные книги, и выглядит обычной адресной книгой: отличить его можно
// только по правам. Без этого все книги считались доступными на запись, и
// правка контакта из каталога оседала локально, а PUT наверх вечно получал
// отказ.
func (c *Client) BookWritable(ctx context.Context, path string) (writable, known bool) {
	if c.httpClient == nil {
		return false, false
	}
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", c.buildFullURL(path), strings.NewReader(privilegeProbeBody))
	if err != nil {
		return false, false
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("CardDAV %s: privilege probe failed: %v", path, err)
		return false, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusMultiStatus {
		return false, false
	}
	return parsePrivilegeSet(body)
}

// parsePrivilegeSet ищет в ответе PROPFIND успешный current-user-privilege-set
// и решает, есть ли право писать карточки: DAV:all, DAV:write или
// DAV:write-content (RFC 3744 §3: write включает write-content).
func parsePrivilegeSet(body []byte) (writable, known bool) {
	var ms struct {
		Responses []struct {
			Propstats []struct {
				Status string `xml:"DAV: status"`
				Prop   struct {
					Set *struct {
						Privileges []struct {
							All          *struct{} `xml:"DAV: all"`
							Write        *struct{} `xml:"DAV: write"`
							WriteContent *struct{} `xml:"DAV: write-content"`
						} `xml:"DAV: privilege"`
					} `xml:"DAV: current-user-privilege-set"`
				} `xml:"DAV: prop"`
			} `xml:"DAV: propstat"`
		} `xml:"DAV: response"`
	}
	if err := xml.Unmarshal(body, &ms); err != nil {
		return false, false
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstats {
			if ps.Prop.Set == nil || !statusOK(ps.Status) {
				continue
			}
			known = true
			for _, p := range ps.Prop.Set.Privileges {
				if p.All != nil || p.Write != nil || p.WriteContent != nil {
					return true, true
				}
			}
		}
	}
	return false, known
}

// statusOK — статус propstat «200». Пустой статус считаем успехом: свойство
// пришло, сервер просто не подписал его.
func statusOK(status string) bool {
	status = strings.TrimSpace(status)
	return status == "" || strings.Contains(status, " 200")
}

// ErrRemoteGone — карточки на сервере больше нет (404/410 на GET).
var ErrRemoteGone = errors.New("carddav: remote contact is gone")

// ErrForbidden — сервер отказал в записи (403): книга только для чтения или
// карточка ему не понравилась. Повтор того же запроса не поможет.
var ErrForbidden = errors.New("carddav: write forbidden")

// maxConflictRetries — сколько раз перечитывать карточку, если её успели
// поменять между нашим GET и PUT (412 на If-Match).
const maxConflictRetries = 3

// PatchContact применяет правку к карточке на сервере без потерь: читает её
// свежую версию (GET), меняет в ней только нужные поля (edit) и пишет PUT с
// If-Match по ETag прочитанной версии. Если между чтением и записью карточку
// поменяли (412), всё повторяется на новой версии — чужая правка, сделанная в
// iCloud за это время, не затирается.
//
// edit получает тело карточки как есть, байт в байт, и возвращает новое.
// Возвращает итоговое тело карточки на сервере.
func (c *Client) PatchContact(ctx context.Context, remotePath string, edit func(string) (string, error)) (string, error) {
	if c.httpClient == nil {
		return "", fmt.Errorf("client not connected")
	}
	url := c.buildFullURL(remotePath)
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		current, etag, err := c.getContact(ctx, url)
		if err != nil {
			return "", err
		}
		next, err := edit(current)
		if err != nil {
			return "", err
		}
		if next == current {
			return current, nil // на сервере уже то же самое
		}
		status, body, err := c.putContact(ctx, url, next, etag)
		if err != nil {
			return "", err
		}
		switch {
		case status == http.StatusPreconditionFailed:
			log.Printf("CardDAV %s: changed on the server between GET and PUT, retrying", remotePath)
			continue
		case status == http.StatusForbidden:
			return "", fmt.Errorf("%w: PUT %s: %s", ErrForbidden, remotePath, body)
		case status >= 400:
			return "", fmt.Errorf("PUT failed with status %d: %s", status, body)
		}
		return next, nil
	}
	return "", fmt.Errorf("PUT %s: still conflicting after %d attempts", remotePath, maxConflictRetries)
}

func (c *Client) getContact(ctx context.Context, url string) (data, etag string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create GET request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("GET request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read GET response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return "", "", ErrRemoteGone
	case resp.StatusCode != http.StatusOK:
		return "", "", fmt.Errorf("GET failed with status %d: %s", resp.StatusCode, snippet(body))
	}
	return string(body), resp.Header.Get("ETag"), nil
}

// putContact пишет карточку. Пустой etag — без If-Match: сервер не отдал ETag
// на GET, и условной записи сделать не из чего.
func (c *Client) putContact(ctx context.Context, url, data, etag string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(data))
	if err != nil {
		return 0, "", fmt.Errorf("failed to create PUT request: %w", err)
	}
	req.Header.Set("Content-Type", "text/vcard; charset=utf-8")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("PUT request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, snippet(body), nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
