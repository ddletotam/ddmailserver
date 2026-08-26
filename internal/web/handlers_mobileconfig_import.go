package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/mobileconfig"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/notify"
)

// maxProfileSize caps an uploaded profile. Real ones are a few kilobytes; this
// is loose enough for a profile carrying certificates and tight enough that the
// endpoint cannot be used to push megabytes through the parser.
const maxProfileSize = 1 << 20 // 1 MiB

// MobileconfigPreview is what the import form is built from: everything the
// profile describes, plus whatever the server needs the user to decide before
// anything is written.
type MobileconfigPreview struct {
	DisplayName  string `json:"display_name,omitempty"`
	Organization string `json:"organization,omitempty"`

	Mail    *MailPreview `json:"mail,omitempty"`
	CalDAV  *DAVPreview  `json:"caldav,omitempty"`
	CardDAV *DAVPreview  `json:"carddav,omitempty"`

	// SuggestedEmail is the address read out of the profile; empty means the
	// profile does not carry one and the user has to supply it, since every
	// source on this server belongs to a concrete identity.
	SuggestedEmail string `json:"suggested_email"`
	EmailRequired  bool   `json:"email_required"`
	SuggestedName  string `json:"suggested_name"`

	// Conflict is set when an account for this address already exists. The
	// user picks replace / abort / rename before anything is written.
	Conflict *ConflictPreview `json:"conflict,omitempty"`

	// Ignored lists payload types this server has no use for, so the user is
	// told what was skipped rather than left to assume it was applied.
	Ignored []string `json:"ignored,omitempty"`

	// Profile is the uploaded document, base64, handed back so the apply call
	// works on exactly the bytes that were previewed rather than a re-upload
	// that might differ.
	Profile string `json:"profile"`
}

// MailPreview describes the mail payload without exposing the credential.
type MailPreview struct {
	EmailAddress string `json:"email_address"`
	AccountName  string `json:"account_name,omitempty"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	IMAPUsername string `json:"imap_username"`
	IMAPTLS      bool   `json:"imap_tls"`
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPUsername string `json:"smtp_username"`
	SMTPTLS      bool   `json:"smtp_tls"`
	HasPassword  bool   `json:"has_password"`
}

// DAVPreview describes a CalDAV or CardDAV payload.
type DAVPreview struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	HasPassword bool   `json:"has_password"`
}

// ConflictPreview describes the account already occupying this address.
type ConflictPreview struct {
	AccountID int64  `json:"account_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	IMAPHost  string `json:"imap_host"`
}

// HandlePreviewMobileconfig parses an uploaded profile and reports what
// importing it would do — without writing anything.
//
// Separating preview from apply is what makes the conflict question answerable:
// the user sees the clash while the answer can still change the outcome, rather
// than after half the profile has landed.
func (s *Server) HandlePreviewMobileconfig(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	data, err := readProfileUpload(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	parsed, err := mobileconfig.Parse(data)
	if err != nil {
		// Signed and POP profiles get their own message: both are ordinary
		// things to be handed, and "invalid profile" would send the user
		// looking for a corruption that isn't there.
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	preview := &MobileconfigPreview{
		DisplayName:  parsed.DisplayName,
		Organization: parsed.Organization,
		Ignored:      parsed.Ignored,
		Profile:      base64.StdEncoding.EncodeToString(data),
	}

	if m := parsed.Mail; m != nil {
		preview.Mail = &MailPreview{
			EmailAddress: m.EmailAddress,
			AccountName:  m.AccountName,
			IMAPHost:     m.IMAPHost,
			IMAPPort:     m.IMAPPort,
			IMAPUsername: m.IMAPUsername,
			IMAPTLS:      m.IMAPTLS,
			SMTPHost:     m.SMTPHost,
			SMTPPort:     m.SMTPPort,
			SMTPUsername: m.SMTPUsername,
			SMTPTLS:      m.SMTPTLS,
			HasPassword:  m.IMAPPassword != "",
		}
	}
	if c := parsed.CalDAV; c != nil {
		preview.CalDAV = &DAVPreview{URL: c.URL(), Username: c.Username, HasPassword: c.Password != ""}
	}
	if c := parsed.CardDAV; c != nil {
		preview.CardDAV = &DAVPreview{URL: c.URL(), Username: c.Username, HasPassword: c.Password != ""}
	}

	email := parsed.ResolvedEmail()
	preview.SuggestedEmail = email
	preview.EmailRequired = email == ""
	preview.SuggestedName = parsed.SuggestedName(email)

	if email != "" {
		existing, err := s.database.FindAccountByEmail(userID, email)
		if err != nil {
			log.Printf("mobileconfig preview: conflict check failed for user %d: %v", userID, err)
			respondError(w, http.StatusInternalServerError, "failed to check for existing accounts")
			return
		}
		if existing != nil {
			preview.Conflict = &ConflictPreview{
				AccountID: existing.ID,
				Email:     existing.Email,
				Name:      existing.Name,
				IMAPHost:  existing.IMAPHost,
			}
		}
	}

	respondJSON(w, http.StatusOK, preview)
}

// ImportMobileconfigRequest is the apply call: the previewed bytes plus the
// decisions the preview asked for.
type ImportMobileconfigRequest struct {
	// Profile is the base64 document returned by the preview.
	Profile string `json:"profile"`

	// Email overrides the address, and is required when the profile has none.
	Email string `json:"email"`

	// Name labels the imported account.
	Name string `json:"name"`

	// Strategy is "abort", "replace" or "rename". Only consulted on conflict.
	Strategy string `json:"strategy"`

	// RenameEmail is the address to import under when Strategy is "rename".
	RenameEmail string `json:"rename_email"`
}

// HandleImportMobileconfig applies a previewed profile.
//
// All or nothing: the account, the calendar source and the contact source are
// written in one transaction, so a profile never lands half-applied.
func (s *Server) HandleImportMobileconfig(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var req ImportMobileconfigRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxProfileSize*2)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	data, err := base64.StdEncoding.DecodeString(req.Profile)
	if err != nil || len(data) == 0 {
		respondError(w, http.StatusBadRequest, "profile is missing or not valid base64")
		return
	}
	if len(data) > maxProfileSize {
		respondError(w, http.StatusRequestEntityTooLarge, "profile is too large")
		return
	}

	parsed, err := mobileconfig.Parse(data)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	email := strings.TrimSpace(req.Email)
	if email == "" {
		email = parsed.ResolvedEmail()
	}
	if email == "" {
		respondError(w, http.StatusBadRequest,
			"this profile carries no email address; supply one to attach the imported sources to")
		return
	}
	if !strings.Contains(email, "@") {
		respondError(w, http.StatusBadRequest, "email address is not valid")
		return
	}

	plan := &db.ImportPlan{
		UserID:        userID,
		IdentityEmail: email,
		Name:          defaultIfEmpty(strings.TrimSpace(req.Name), parsed.SuggestedName(email)),
		Strategy:      db.ConflictStrategy(defaultIfEmpty(req.Strategy, string(db.ConflictAbort))),
		RenameEmail:   strings.TrimSpace(req.RenameEmail),
	}

	if m := parsed.Mail; m != nil {
		plan.Account = &models.Account{
			UserID:       userID,
			Name:         plan.Name,
			Email:        email,
			IMAPHost:     m.IMAPHost,
			IMAPPort:     m.IMAPPort,
			IMAPUsername: defaultIfEmpty(m.IMAPUsername, email),
			IMAPPassword: m.IMAPPassword,
			IMAPTLS:      m.IMAPTLS,
			SMTPHost:     m.SMTPHost,
			SMTPPort:     m.SMTPPort,
			SMTPUsername: defaultIfEmpty(m.SMTPUsername, email),
			SMTPPassword: m.SMTPPassword,
			SMTPTLS:      m.SMTPTLS,
			Enabled:      true,
			AuthType:     "password",
		}
	}

	if c := parsed.CalDAV; c != nil {
		plan.CalendarSource = &models.CalendarSource{
			Name:           plan.Name,
			SourceType:     "caldav",
			CalDAVURL:      c.URL(),
			CalDAVUsername: defaultIfEmpty(c.Username, email),
			CalDAVPassword: c.Password,
			AuthType:       "password",
			SyncEnabled:    true,
			SyncInterval:   300,
			Color:          "#3788d8",
		}
	}

	if c := parsed.CardDAV; c != nil {
		plan.ContactSource = &models.ContactSource{
			Name:            plan.Name,
			SourceType:      "carddav",
			CardDAVURL:      c.URL(),
			CardDAVUsername: defaultIfEmpty(c.Username, email),
			CardDAVPassword: c.Password,
			AuthType:        "password",
			SyncEnabled:     true,
			SyncInterval:    300,
		}
	}

	result, err := s.database.ApplyImportPlan(plan)
	if err != nil {
		if errors.Is(err, db.ErrImportAborted) {
			// Not a server failure: the user asked to cancel, and nothing was
			// written. 409 so the UI can say so plainly.
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("mobileconfig import: failed for user %d: %v", userID, err)
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	log.Printf("mobileconfig import: user %d — account %s (id %d), calendar source %d, contact source %d",
		userID, result.AccountAction, result.AccountID, result.CalendarSourceID, result.ContactSourceID)

	// Tell any connected desktop client that its account set just changed.
	// Identities are cached there and refreshed only on a full sync, so
	// without this the imported account would stay invisible until restart.
	if s.notifyHub != nil {
		s.notifyHub.Publish(notify.Event{
			UserID: userID,
			Type:   notify.EventIdentitiesChanged,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// readProfileUpload accepts the document either as a multipart file upload or
// as a raw body, so the endpoint works from a form and from curl alike.
func readProfileUpload(r *http.Request) ([]byte, error) {
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxProfileSize); err != nil {
			return nil, fmt.Errorf("failed to read upload: %w", err)
		}
		file, _, err := r.FormFile("profile")
		if err != nil {
			return nil, fmt.Errorf("no file named \"profile\" in the upload")
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, maxProfileSize+1))
		if err != nil {
			return nil, fmt.Errorf("failed to read upload: %w", err)
		}
		if len(data) > maxProfileSize {
			return nil, fmt.Errorf("profile is too large")
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("profile is empty")
		}
		return data, nil
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxProfileSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	if len(data) > maxProfileSize {
		return nil, fmt.Errorf("profile is too large")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("profile is empty")
	}
	return data, nil
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
