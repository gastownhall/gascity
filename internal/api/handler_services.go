package api

import (
	"net"
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/workspacesvc"
)

func (s *Server) handleServiceProxy(w http.ResponseWriter, r *http.Request) {
	reg := s.state.ServiceRegistry()
	if reg == nil {
		writeProblemDetails(w, http.StatusNotFound, problemDetailsTitle(http.StatusNotFound), "not_found: service route not found")
		return
	}
	name := serviceNameFromPath(r.URL.Path)
	if name == "" {
		writeProblemDetails(w, http.StatusNotFound, problemDetailsTitle(http.StatusNotFound), "not_found: service route not found")
		return
	}
	if !reg.AuthorizeAndServeHTTP(name, w, r, func(status workspacesvc.Status) bool {
		return serviceRequestAllowed(w, status, r, s.readOnly)
	}) {
		writeProblemDetails(w, http.StatusNotFound, problemDetailsTitle(http.StatusNotFound), "not_found: service route not found")
	}
}

func serviceNameFromPath(path string) string {
	path = strings.TrimPrefix(path, "/svc/")
	if path == "" {
		return ""
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

func serviceRequestAllowed(w http.ResponseWriter, status workspacesvc.Status, r *http.Request, apiReadOnly bool) bool {
	if status.PublishMode == "" {
		status.PublishMode = "private"
	}
	// The raw controller listener only relaxes ingress guards for legacy
	// direct publication. Hosted/publication routes use a separate edge and
	// should not become public merely because a status projection synthesized a
	// published URL.
	directPublished := status.PublishMode == "direct"
	if apiReadOnly && !directPublished && isMutationMethod(r.Method) {
		writeProblemDetails(w, http.StatusForbidden, problemDetailsTitle(http.StatusForbidden), "read_only: service mutations are disabled for unpublished services")
		return false
	}
	if !directPublished {
		internalProxyRequest := r.Header.Get("X-GC-Request") != ""
		if !isLoopbackRemoteAddr(r.RemoteAddr) && !internalProxyRequest {
			writeProblemDetails(w, http.StatusNotFound, problemDetailsTitle(http.StatusNotFound), "not_found: service route not found")
			return false
		}
		if isMutationMethod(r.Method) && !internalProxyRequest {
			writeProblemDetails(w, http.StatusForbidden, problemDetailsTitle(http.StatusForbidden), "csrf: X-GC-Request header required on private service mutation endpoints")
			return false
		}
	}
	return true
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
