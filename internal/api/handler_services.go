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
		problemServiceRouteNotFound.writeTo(w)
		return
	}
	name := serviceNameFromPath(r.URL.Path)
	if name == "" {
		problemServiceRouteNotFound.writeTo(w)
		return
	}
	if !reg.AuthorizeAndServeHTTP(name, w, r, func(status workspacesvc.Status) bool {
		return serviceRequestAllowed(w, status, r, s.readOnly)
	}) {
		problemServiceRouteNotFound.writeTo(w)
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
		problemServiceReadOnly.writeTo(w)
		return false
	}
	if !directPublished {
		internalProxyRequest := r.Header.Get("X-GC-Request") != ""
		if !isLoopbackRemoteAddr(r.RemoteAddr) && !internalProxyRequest {
			problemServiceRouteNotFound.writeTo(w)
			return false
		}
		if isMutationMethod(r.Method) && !internalProxyRequest {
			problemServiceCSRFRequired.writeTo(w)
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
