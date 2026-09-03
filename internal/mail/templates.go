package mail

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed templates/*.txt templates/*.html
var templateFS embed.FS

// TemplateIDs — one per transactional mail. Every template exists in both a
// .txt and an .html variant; the renderer always produces both bodies.
const (
	TemplateVerifyCode         = "verify_code"
	TemplatePasswordReset      = "password_reset"
	TemplateEmailChange        = "email_change"
	TemplateNewDevice          = "new_device"
	TemplatePasswordChange     = "password_change"
	TemplateAccountDeletion    = "account_deletion"
	TemplateEmailChangedNotice = "email_changed_notice"
)

// Message is a fully rendered outgoing mail.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Common data every template receives. Values appear in mail only — never
// in logs (see senders: bodies are never logged).
type templateData struct {
	// Verification / reset codes and links.
	Code string
	Link string
	// ResetLinkMinutes / VerifyMinutes drive the copy.
	ResetLinkMinutes int
	VerifyMinutes    int
	// Device & account context for the security notices.
	DeviceName string
	AppVersion string
	LoginAt    string
	// EmailChange: old → new.
	OldEmail string
	NewEmail string
	// AccountDeletion.
	RequestedAt string
	// Pre-escaped values for the HTML templates (exported: text/template
	// can only reach exported fields).
	CodeHTML string
	LinkHTML string
}

var tplCache = map[string]*template.Template{}

func tplFor(id, ext string) *template.Template {
	key := id + "." + ext
	if t, ok := tplCache[key]; ok {
		return t
	}
	t := template.Must(template.ParseFS(templateFS, "templates/"+key))
	tplCache[key] = t
	return t
}

// Render produces the dual-body message for a template. Unknown IDs panic
// at call time only when the template files are missing — they are embedded,
// so a missing file is a build-time authoring bug, caught on first use in
// tests/dev (the same contract as html/template.Must in admin).
func Render(id string, data *templateData) (*Message, error) {
	if data == nil {
		data = &templateData{}
	}
	// Pre-escape values that appear inside HTML anchors/code blocks. The
	// text templates receive the raw values.
	data.CodeHTML = template.HTMLEscapeString(data.Code)
	data.LinkHTML = template.HTMLEscapeString(data.Link)

	var textBuf, htmlBuf strings.Builder
	if err := tplFor(id, "txt").Execute(&textBuf, data); err != nil {
		return nil, fmt.Errorf("mail template %s.txt: %w", id, err)
	}
	if err := tplFor(id, "html").Execute(&htmlBuf, data); err != nil {
		return nil, fmt.Errorf("mail template %s.html: %w", id, err)
	}
	return &Message{Subject: subjectFor(id), Text: textBuf.String(), HTML: htmlBuf.String()}, nil
}

func subjectFor(id string) string {
	switch id {
	case TemplateVerifyCode:
		return "LiveTranslate 验证码 / Verification code"
	case TemplatePasswordReset:
		return "LiveTranslate 密码重置 / Password reset"
	case TemplateEmailChange:
		return "LiveTranslate 邮箱验证码 / Email change code"
	case TemplateNewDevice:
		return "LiveTranslate 新设备登录 / New device sign-in"
	case TemplatePasswordChange:
		return "LiveTranslate 密码已修改 / Password changed"
	case TemplateAccountDeletion:
		return "LiveTranslate 账号删除 / Account deletion"
	case TemplateEmailChangedNotice:
		return "LiveTranslate 登录邮箱已变更 / Sign-in email changed"
	default:
		return "LiveTranslate"
	}
}
