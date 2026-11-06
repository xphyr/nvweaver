package api

import (
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"nvweaver/internal/auth"
)

const sessionCookie = "nvweaver_session"

type sessionView struct {
	Method   string `json:"method"`
	SignedIn bool   `json:"signed_in"`
	Username string `json:"username,omitempty"`
}

func writeNoStore(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, v)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) error {
	view := sessionView{Method: "none", SignedIn: true}
	switch {
	case s.Auth != nil:
		view.Method = "password"
	case s.token != "":
		view.Method = "token"
	}
	if view.Method != "none" {
		view.SignedIn = s.signedIn(r)
		view.Username, _ = s.sessionUser(r)
	}
	writeNoStore(w, http.StatusOK, view)
	return nil
}

// connectionIsProtected reports whether a password would stay private: over TLS, or to this machine. It uses the address that the
// connection really arrived on, as anything in the request itself can say whatever the sender likes.
func connectionIsProtected(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

func (s *Server) signIn(w http.ResponseWriter, r *http.Request) error {
	if s.Auth == nil {
		return notFound("this service has no users to sign in as: set NVWEAVER_USERS_FILE")
	}
	if !s.AllowInsecureLogin && !connectionIsProtected(r) {
		writeNoStore(w, http.StatusForbidden, map[string]string{
			"error": "refusing a password over plain HTTP, which anyone on the network could read: set NVWEAVER_TLS_CERT and NVWEAVER_TLS_KEY, or NVWEAVER_ALLOW_INSECURE_LOGIN=true to accept that",
		})
		return nil
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, expires, err := s.Auth.Login(auth.ClientOf(r.RemoteAddr), in.Username, in.Password)
	var many *auth.TooManyError
	switch {
	case errors.As(err, &many):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(many.RetryAfter.Seconds()))))
		writeNoStore(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return nil
	case errors.Is(err, auth.ErrWrongCredentials):
		writeNoStore(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return nil
	case err != nil:
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	writeNoStore(w, http.StatusOK, sessionView{Method: "password", SignedIn: true, Username: in.Username})
	return nil
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request) error {
	if cookie, err := r.Cookie(sessionCookie); err == nil && s.Auth != nil {
		s.Auth.Logout(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	w.WriteHeader(http.StatusNoContent)
	return nil
}
