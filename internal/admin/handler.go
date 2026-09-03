package admin

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Handler renders the admin pages. All state-changing routes require the
// session cookie AND a matching csrf_token form field (double submit).
type Handler struct {
	svc *Service
	cfg *config.Config
	tpl *template.Template
}

func NewHandler(cfg *config.Config, svc *Service) *Handler {
	tpl := template.Must(template.New("").Funcs(template.FuncMap{
		"fmtTime": func(t time.Time) string {
			return t.Local().Format("2006-01-02 15:04")
		},
		"fmtTimePtr": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"fmtEmailPtr": func(e *string) string {
			if e == nil {
				return "(无邮箱 / Apple 或开发者账号)"
			}
			return *e
		},
	}).ParseFS(templateFS, "templates/*.html"))
	return &Handler{svc: svc, cfg: cfg, tpl: tpl}
}

// Register mounts everything under mux (run on its own listener).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("POST /login", h.loginSubmit)
	mux.HandleFunc("POST /logout", h.logout)
	mux.HandleFunc("GET /{$}", h.dashboard)
	mux.HandleFunc("GET /users", h.usersPage)
	mux.HandleFunc("GET /users/{id}", h.userDetailPage)
	mux.HandleFunc("POST /users/{id}/suspend", h.userAction("suspend"))
	mux.HandleFunc("POST /users/{id}/reactivate", h.userAction("reactivate"))
	mux.HandleFunc("POST /users/{id}/force-logout", h.userAction("force-logout"))
	mux.HandleFunc("POST /users/{id}/delete", h.userAction("delete"))
	mux.HandleFunc("GET /invitations", h.invitationsPage)
	mux.HandleFunc("POST /invitations", h.invitationCreate)
	mux.HandleFunc("POST /invitations/revoke", h.invitationRevoke)
	mux.HandleFunc("GET /audit", h.auditPage)
}

// securityHeaders are page-level additions on top of httpapi.Handler's set.
func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'none'; frame-ancestors 'none'")
}

// page is the data every template gets.
type page struct {
	Title string
	Admin bool // logged in?
	CSRF  string
	Flash string
	Error string
	Data  any
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, p page) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.tpl.ExecuteTemplate(w, name, p); err != nil {
		slog.Error("template render failed", "template", name, "err", err.Error())
	}
}

// render500 logs the underlying error and shows the admin a generic page —
// internal error details never reach the browser.
func (h *Handler) render500(w http.ResponseWriter, r *http.Request, sess *AdminSession, where string, err error) {
	slog.Error("admin page failed", "where", where, "method", r.Method, "path", r.URL.Path, "err", err.Error())
	p := page{Title: "错误", Error: "数据库错误（操作未执行或已回滚）"}
	if sess != nil {
		p.Admin, p.CSRF = true, sess.CSRFToken
	}
	h.render(w, http.StatusInternalServerError, "error.html", p)
}

// requireSession resolves the admin session or redirects to /login.
func (h *Handler) requireSession(w http.ResponseWriter, r *http.Request) *AdminSession {
	sess := h.svc.ResolveSession(r.Context(), r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	return sess
}

// --- Login -----------------------------------------------------------------------

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if h.svc.ResolveSession(r.Context(), r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render(w, http.StatusOK, "login.html", page{Title: "登录 · LiveTranslate Admin"})
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.render(w, http.StatusBadRequest, "login.html", page{Title: "登录", Error: "表单无效"})
		return
	}
	username := r.PostFormValue("username")
	passwd := r.PostFormValue("password")
	totpCode := r.PostFormValue("totp_code")
	ipHash := auth.HashPII(r.RemoteAddr)
	sessionToken, _, err := h.svc.Login(r.Context(), username, totpCode, passwd, ipHash, r.UserAgent())
	if err != nil {
		h.render(w, http.StatusUnauthorized, "login.html", page{
			Title: "登录", Error: "用户名、密码或验证码错误，或账号已临时锁定"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   !h.cfg.DevMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.cfg.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	sess := h.svc.ResolveSession(r.Context(), r)
	if sess == nil {
		// Already logged out: idempotent redirect.
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !h.svc.ValidCSRF(sess, r) {
		h.render(w, http.StatusForbidden, "error.html", page{
			Title: "错误", Admin: true, CSRF: sess.CSRFToken, Error: "CSRF 校验失败"})
		return
	}
	_ = h.svc.Logout(r.Context(), r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Dashboard ---------------------------------------------------------------------

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	events, err := h.svc.AuditFeed(r.Context())
	if err != nil {
		h.render500(w, r, sess, "audit_feed", err)
		return
	}
	h.render(w, http.StatusOK, "dashboard.html", page{
		Title: "概览", Admin: true, CSRF: sess.CSRFToken, Data: events})
}

// --- Users ---------------------------------------------------------------------------

func (h *Handler) usersPage(w http.ResponseWriter, r *http.Request) {
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	search := r.URL.Query().Get("q")
	pageNo, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if pageNo < 1 {
		pageNo = 1
	}
	users, total, err := h.svc.ListUsers(r.Context(), search, pageNo)
	if err != nil {
		h.render500(w, r, sess, "list_users", err)
		return
	}
	h.render(w, http.StatusOK, "users.html", page{
		Title: "用户", Admin: true, CSRF: sess.CSRFToken,
		Data: map[string]any{"Users": users, "Total": total, "Page": pageNo, "Q": search},
	})
}

func (h *Handler) userDetailPage(w http.ResponseWriter, r *http.Request) {
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u, err := h.svc.UserDetail(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		h.render500(w, r, sess, "user_detail", err)
		return
	}
	h.render(w, http.StatusOK, "user_detail.html", page{
		Title: "用户详情", Admin: true, CSRF: sess.CSRFToken, Data: u})
}

// userAction is the shared POST dispatcher for the per-user operations.
// Every action is audited with the admin's reason.
func (h *Handler) userAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := h.requireSession(w, r)
		if sess == nil {
			return
		}
		if !h.svc.ValidCSRF(sess, r) {
			h.render(w, http.StatusForbidden, "error.html", page{
				Title: "错误", Admin: true, CSRF: sess.CSRFToken, Error: "CSRF 校验失败"})
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		reason := r.PostFormValue("reason")

		var opErr error
		switch action {
		case "suspend":
			opErr = h.svc.SuspendUser(r.Context(), sess.AdminID, id, reason)
		case "reactivate":
			opErr = h.svc.ReactivateUser(r.Context(), sess.AdminID, id, reason)
		case "force-logout":
			opErr = h.svc.ForceLogout(r.Context(), sess.AdminID, id, reason)
		case "delete":
			confirm := r.PostFormValue("confirm")
			if confirm != "DELETE" {
				opErr = errConfirmRequired
			} else {
				opErr = h.svc.DeleteUser(r.Context(), sess.AdminID, id, reason)
			}
		}
		if opErr != nil {
			if errors.Is(opErr, errConfirmRequired) {
				h.render(w, http.StatusBadRequest, "error.html", page{
					Title: "操作失败", Admin: true, CSRF: sess.CSRFToken,
					Error: opErr.Error()})
				return
			}
			slog.Error("admin user action failed", "action", action,
				"path", r.URL.Path, "err", opErr.Error())
			h.render(w, http.StatusInternalServerError, "error.html", page{
				Title: "操作失败", Admin: true, CSRF: sess.CSRFToken,
				Error: "操作未执行或已回滚"})
			return
		}
		http.Redirect(w, r, "/users/"+id.String(), http.StatusSeeOther)
	}
}

var errConfirmRequired = &confirmError{}

type confirmError struct{}

func (*confirmError) Error() string { return "删除账号需要在确认框输入 DELETE" }

// --- Invitations -----------------------------------------------------------------------

func (h *Handler) invitationsPage(w http.ResponseWriter, r *http.Request) {
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	invs, err := h.svc.InvitationList(r.Context())
	if err != nil {
		h.render500(w, r, sess, "invitation_list", err)
		return
	}
	h.render(w, http.StatusOK, "invitations.html", page{
		Title: "邀请码", Admin: true, CSRF: sess.CSRFToken, Data: invs})
}

func (h *Handler) invitationCreate(w http.ResponseWriter, r *http.Request) {
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	if !h.svc.ValidCSRF(sess, r) {
		h.render(w, http.StatusForbidden, "error.html", page{
			Title: "错误", Admin: true, CSRF: sess.CSRFToken, Error: "CSRF 校验失败"})
		return
	}
	_ = r.ParseForm()
	note := r.PostFormValue("note")
	maxUses, _ := strconv.Atoi(r.PostFormValue("max_uses"))
	if maxUses < 1 || maxUses > 100 {
		maxUses = 1
	}
	ttl := 14 * 24 * time.Hour
	code, err := h.svc.CreateInvitation(r.Context(), sess.AdminID, note, maxUses, ttl)
	if err != nil {
		h.render500(w, r, sess, "invitation_create", err)
		return
	}
	http.Redirect(w, r, "/invitations?created="+code, http.StatusSeeOther)
}

func (h *Handler) invitationRevoke(w http.ResponseWriter, r *http.Request) {
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	if !h.svc.ValidCSRF(sess, r) {
		h.render(w, http.StatusForbidden, "error.html", page{
			Title: "错误", Admin: true, CSRF: sess.CSRFToken, Error: "CSRF 校验失败"})
		return
	}
	_ = r.ParseForm()
	_ = h.svc.RevokeInvitation(r.Context(), sess.AdminID, r.PostFormValue("code"))
	http.Redirect(w, r, "/invitations", http.StatusSeeOther)
}

// --- Audit -----------------------------------------------------------------------------

func (h *Handler) auditPage(w http.ResponseWriter, r *http.Request) {
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	events, err := h.svc.AuditFeed(r.Context())
	if err != nil {
		h.render500(w, r, sess, "audit_feed", err)
		return
	}
	h.render(w, http.StatusOK, "audit.html", page{
		Title: "审计日志", Admin: true, CSRF: sess.CSRFToken, Data: events})
}
