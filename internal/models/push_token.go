package models

// Службы доставки push (столбец push_tokens.kind).
const (
	PushKindAPNs = "apns"
	PushKindHMS  = "hms"
	PushKindFCM  = "fcm"
)

// Окружения APNs (столбец push_tokens.environment). Токен отладочной сборки
// принимает только sandbox, релизной — только production.
const (
	PushEnvSandbox    = "sandbox"
	PushEnvProduction = "production"
)

// PushToken — устройство пользователя, которому можно послать push
// (migrations/055_push_tokens.sql).
type PushToken struct {
	ID          int64
	UserID      int64
	Kind        string // apns, hms, fcm
	Token       string
	Environment string // APNs: sandbox | production; HMS/FCM: ""
	Topic       string // APNs: bundle id; "" — из конфига
	CreatedAt   int64  // ms
	UpdatedAt   int64  // ms
}
