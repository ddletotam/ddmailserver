package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/web"
)

// createAdminAccount makes an administrator account (migration 061): the
// password comes from the first line of in; an empty line generates one,
// printed to out once — it is not stored anywhere else.
func createAdminAccount(database *db.DB, username string, in io.Reader, out io.Writer) error {
	username = strings.TrimSpace(username)
	if username == "" || strings.ContainsAny(username, " @") {
		return errors.New("login must be non-empty, without spaces or @")
	}
	if _, err := database.GetUserByUsername(username); err == nil {
		return fmt.Errorf("login %q is taken", username)
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	generated := password == ""
	if generated {
		if password, err = web.RandomPassword(20); err != nil {
			return err
		}
	} else if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := web.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	u, err := database.CreateAdmin(username, hash)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "administrator %q created (id %d)\n", u.Username, u.ID)
	if generated {
		fmt.Fprintf(out, "password: %s\n", password)
	}
	return nil
}
