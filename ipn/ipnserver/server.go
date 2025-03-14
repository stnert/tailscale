// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipnserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnauth"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/localapi"
	"tailscale.com/types/logger"
	"tailscale.com/util/mak"
	"tailscale.com/util/set"
	"tailscale.com/util/systemd"
)

// Server is an IPN backend and its set of 0 or more active localhost
// TCP or unix socket connections talking to that backend.
type Server struct {
	lb           atomic.Pointer[ipnlocal.LocalBackend]
	logf         logger.Logf
	backendLogID string
	// resetOnZero is whether to call bs.Reset on transition from
	// 1->0 active HTTP requests. That is, this is whether the backend is
	// being run in "client mode" that requires an active GUI
	// connection (such as on Windows by default). Even if this
	// is true, the ForceDaemon pref can override this.
	resetOnZero bool

	startBackendOnce sync.Once
	runCalled        atomic.Bool

	// mu guards the fields that follow.
	// lock order: mu, then LocalBackend.mu
	mu            sync.Mutex
	lastUserID    ipn.WindowsUserID // tracks last userid; on change, Reset state for paranoia
	activeReqs    map[*http.Request]*ipnauth.ConnIdentity
	backendWaiter waiterSet // of LocalBackend waiters
	zeroReqWaiter waiterSet // of blockUntilZeroConnections waiters
}

func (s *Server) mustBackend() *ipnlocal.LocalBackend {
	lb := s.lb.Load()
	if lb == nil {
		panic("unexpected: call to mustBackend in path where SetLocalBackend should've been called")
	}
	return lb
}

// waiterSet is a set of callers waiting on something. Each item (map value) in
// the set is a func that wakes up that waiter's context. The waiter is responsible
// for removing itself from the set when woken up. The (*waiterSet).add method
// returns a cleanup method which does that removal. The caller than defers that
// cleanup.
//
// TODO(bradfitz): this is a generally useful pattern. Move elsewhere?
type waiterSet set.HandleSet[context.CancelFunc]

// add registers a new waiter in the set.
// It aquires mu to add the waiter, and does so again when cleanup is called to remove it.
// ready is closed when the waiter is ready (or ctx is done).
func (s *waiterSet) add(mu *sync.Mutex, ctx context.Context) (ready <-chan struct{}, cleanup func()) {
	ctx, cancel := context.WithCancel(ctx)
	hs := (*set.HandleSet[context.CancelFunc])(s) // change method set
	mu.Lock()
	h := hs.Add(cancel)
	mu.Unlock()
	return ctx.Done(), func() {
		mu.Lock()
		delete(*hs, h)
		mu.Unlock()
		cancel()
	}
}

// wakeAll wakes up all waiters in the set.
func (w waiterSet) wakeAll() {
	for _, cancel := range w {
		cancel() // they'll remove themselves
	}
}

func (s *Server) awaitBackend(ctx context.Context) (_ *ipnlocal.LocalBackend, ok bool) {
	lb := s.lb.Load()
	if lb != nil {
		return lb, true
	}

	ready, cleanup := s.backendWaiter.add(&s.mu, ctx)
	defer cleanup()

	// Try again, now that we've registered, in case there was a
	// race.
	lb = s.lb.Load()
	if lb != nil {
		return lb, true
	}

	<-ready
	lb = s.lb.Load()
	return lb, lb != nil
}

// serveServerStatus serves the /server-status endpoint which reports whether
// the LocalBackend is up yet.
// This is primarily for the Windows GUI, because wintun can take awhile to
// come up. See https://github.com/tailscale/tailscale/issues/6522.
func (s *Server) serveServerStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	w.Header().Set("Content-Type", "application/json")
	var res struct {
		Error string `json:"error,omitempty"`
	}

	lb := s.lb.Load()
	if lb == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		if wait, _ := strconv.ParseBool(r.FormValue("wait")); wait {
			w.(http.Flusher).Flush()
			lb, _ = s.awaitBackend(ctx)
		}
	}

	if lb == nil {
		res.Error = "backend not ready"
	}
	json.NewEncoder(w).Encode(res)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == "CONNECT" {
		if envknob.GOOS() == "windows" {
			// For the GUI client when using an exit node. See docs on handleProxyConnectConn.
			s.handleProxyConnectConn(w, r)
		} else {
			http.Error(w, "bad method for platform", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for this method before the awaitBackend call, as it reports whether
	// the backend is available.
	if r.Method == "GET" && r.URL.Path == "/server-status" {
		s.serveServerStatus(w, r)
		return
	}

	lb, ok := s.awaitBackend(ctx)
	if !ok {
		// Almost certainly because the context was canceled so the response
		// here doesn't really matter. The client is gone.
		http.Error(w, "no backend", http.StatusServiceUnavailable)
		return
	}

	var ci *ipnauth.ConnIdentity
	switch v := r.Context().Value(connIdentityContextKey{}).(type) {
	case *ipnauth.ConnIdentity:
		ci = v
	case error:
		http.Error(w, v.Error(), http.StatusUnauthorized)
		return
	case nil:
		http.Error(w, "internal error: no connIdentityContextKey", http.StatusInternalServerError)
		return
	}

	onDone, err := s.addActiveHTTPRequest(r, ci)
	if err != nil {
		if ou, ok := err.(inUseOtherUserError); ok && localapi.InUseOtherUserIPNStream(w, r, ou.Unwrap()) {
			w.(http.Flusher).Flush()
			s.blockWhileIdentityInUse(ctx, ci)
			return
		}
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	defer onDone()

	if strings.HasPrefix(r.URL.Path, "/localapi/") {
		lah := localapi.NewHandler(lb, s.logf, s.backendLogID)
		lah.PermitRead, lah.PermitWrite = s.localAPIPermissions(ci)
		lah.PermitCert = s.connCanFetchCerts(ci)
		lah.ServeHTTP(w, r)
		return
	}

	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if envknob.GOOS() == "windows" {
		// TODO(bradfitz): remove this once we moved to named pipes for LocalAPI
		// on Windows. This could then move to all platforms instead at
		// 100.100.100.100 or something (quad100 handler in LocalAPI)
		s.ServeHTMLStatus(w, r)
		return
	}

	io.WriteString(w, "<html><title>Tailscale</title><body><h1>Tailscale</h1>This is the local Tailscale daemon.\n")
}

// inUseOtherUserError is the error type for when the server is in use
// by a different local user.
type inUseOtherUserError struct{ error }

func (e inUseOtherUserError) Unwrap() error { return e.error }

// checkConnIdentityLocked checks whether the provided identity is
// allowed to connect to the server.
//
// The returned error, when non-nil, will be of type inUseOtherUserError.
//
// s.mu must be held.
func (s *Server) checkConnIdentityLocked(ci *ipnauth.ConnIdentity) error {
	// If clients are already connected, verify they're the same user.
	// This mostly matters on Windows at the moment.
	if len(s.activeReqs) > 0 {
		var active *ipnauth.ConnIdentity
		for _, active = range s.activeReqs {
			break
		}
		if active != nil && ci.WindowsUserID() != active.WindowsUserID() {
			return inUseOtherUserError{fmt.Errorf("Tailscale already in use by %s, pid %d", active.User().Username, active.Pid())}
		}
	}
	if err := s.mustBackend().CheckIPNConnectionAllowed(ci); err != nil {
		return inUseOtherUserError{err}
	}
	return nil
}

// blockWhileIdentityInUse blocks while ci can't connect to the server because
// the server is in use by a different user.
//
// This is primarily used for the Windows GUI, to block until one user's done
// controlling the tailscaled process.
func (s *Server) blockWhileIdentityInUse(ctx context.Context, ci *ipnauth.ConnIdentity) error {
	inUse := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.checkConnIdentityLocked(ci).(inUseOtherUserError)
		return ok
	}
	for inUse() {
		// Check whenever the connection count drops down to zero.
		ready, cleanup := s.zeroReqWaiter.add(&s.mu, ctx)
		<-ready
		cleanup()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// localAPIPermissions returns the permissions for the given identity accessing
// the Tailscale local daemon API.
//
// s.mu must not be held.
func (s *Server) localAPIPermissions(ci *ipnauth.ConnIdentity) (read, write bool) {
	switch envknob.GOOS() {
	case "windows":
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.checkConnIdentityLocked(ci) == nil {
			return true, true
		}
		return false, false
	case "js":
		return true, true
	}
	if ci.IsUnixSock() {
		return true, !ci.IsReadonlyConn(s.mustBackend().OperatorUserID(), logger.Discard)
	}
	return false, false
}

// userIDFromString maps from either a numeric user id in string form
// ("998") or username ("caddy") to its string userid ("998").
// It returns the empty string on error.
func userIDFromString(v string) string {
	if v == "" || isAllDigit(v) {
		return v
	}
	u, err := user.Lookup(v)
	if err != nil {
		return ""
	}
	return u.Uid
}

func isAllDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// connCanFetchCerts reports whether ci is allowed to fetch HTTPS
// certs from this server when it wouldn't otherwise be able to.
//
// That is, this reports whether ci should grant additional
// capabilities over what the conn would otherwise be able to do.
//
// For now this only returns true on Unix machines when
// TS_PERMIT_CERT_UID is set the to the userid of the peer
// connection. It's intended to give your non-root webserver access
// (www-data, caddy, nginx, etc) to certs.
func (s *Server) connCanFetchCerts(ci *ipnauth.ConnIdentity) bool {
	if ci.IsUnixSock() && ci.Creds() != nil {
		connUID, ok := ci.Creds().UserID()
		if ok && connUID == userIDFromString(envknob.String("TS_PERMIT_CERT_UID")) {
			return true
		}
	}
	return false
}

// addActiveHTTPRequest adds c to the server's list of active HTTP requests.
//
// If the returned error may be of type inUseOtherUserError.
//
// onDone must be called when the HTTP request is done.
func (s *Server) addActiveHTTPRequest(req *http.Request, ci *ipnauth.ConnIdentity) (onDone func(), err error) {
	if ci == nil {
		return nil, errors.New("internal error: nil connIdentity")
	}

	lb := s.mustBackend()

	// If the connected user changes, reset the backend server state to make
	// sure node keys don't leak between users.
	var doReset bool
	defer func() {
		if doReset {
			s.logf("identity changed; resetting server")
			lb.ResetForClientDisconnect()
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkConnIdentityLocked(ci); err != nil {
		return nil, err
	}

	mak.Set(&s.activeReqs, req, ci)

	if uid := ci.WindowsUserID(); uid != "" && len(s.activeReqs) == 1 {
		// Tell the LocalBackend about the identity we're now running as.
		lb.SetCurrentUserID(uid)
		if s.lastUserID != uid {
			if s.lastUserID != "" {
				doReset = true
			}
			s.lastUserID = uid
		}
	}

	onDone = func() {
		s.mu.Lock()
		delete(s.activeReqs, req)
		remain := len(s.activeReqs)
		s.mu.Unlock()

		if remain == 0 && s.resetOnZero {
			if lb.InServerMode() {
				s.logf("client disconnected; staying alive in server mode")
			} else {
				s.logf("client disconnected; stopping server")
				lb.ResetForClientDisconnect()
			}
		}

		// Wake up callers waiting for the server to be idle:
		if remain == 0 {
			s.mu.Lock()
			s.zeroReqWaiter.wakeAll()
			s.mu.Unlock()
		}
	}

	return onDone, nil
}

// New returns a new Server.
//
// To start it, use the Server.Run method.
//
// At some point, either before or after Run, the Server's SetLocalBackend
// method must also be called before Server can do anything useful.
func New(logf logger.Logf, logid string) *Server {
	return &Server{
		backendLogID: logid,
		logf:         logf,
		resetOnZero:  envknob.GOOS() == "windows",
	}
}

// SetLocalBackend sets the server's LocalBackend.
//
// If b.Run has already been called, then lb.Start will be called.
// Otherwise Start will be called once Run is called.
func (s *Server) SetLocalBackend(lb *ipnlocal.LocalBackend) {
	if lb == nil {
		panic("nil LocalBackend")
	}
	if !s.lb.CompareAndSwap(nil, lb) {
		panic("already set")
	}
	s.startBackendIfNeeded()

	s.mu.Lock()
	s.backendWaiter.wakeAll()
	s.mu.Unlock()

	// TODO(bradfitz): send status update to GUI long poller waiter. See
	// https://github.com/tailscale/tailscale/issues/6522
}

func (b *Server) startBackendIfNeeded() {
	if !b.runCalled.Load() {
		return
	}
	lb := b.lb.Load()
	if lb == nil {
		return
	}
	if lb.Prefs().Valid() {
		b.startBackendOnce.Do(func() {
			lb.Start(ipn.Options{})
		})
	}
}

// connIdentityContextKey is the http.Request.Context's context.Value key for either an
// *ipnauth.ConnIdentity or an error.
type connIdentityContextKey struct{}

// Run runs the server, accepting connections from ln forever.
//
// If the context is done, the listener is closed. It is also the base context
// of all HTTP requests.
//
// If the Server's LocalBackend has already been set, Run starts it.
// Otherwise, the next call to SetLocalBackend will start it.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	s.runCalled.Store(true)
	defer func() {
		if lb := s.lb.Load(); lb != nil {
			lb.Shutdown()
		}
	}()

	runDone := make(chan struct{})
	defer close(runDone)

	// When the context is closed or when we return, whichever is first, close our listener
	// and all open connections.
	go func() {
		select {
		case <-ctx.Done():
		case <-runDone:
		}
		ln.Close()
	}()

	s.startBackendIfNeeded()
	systemd.Ready()

	hs := &http.Server{
		Handler:     http.HandlerFunc(s.serveHTTP),
		BaseContext: func(_ net.Listener) context.Context { return ctx },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			ci, err := ipnauth.GetConnIdentity(s.logf, c)
			if err != nil {
				return context.WithValue(ctx, connIdentityContextKey{}, err)
			}
			return context.WithValue(ctx, connIdentityContextKey{}, ci)
		},
		// Localhost connections are cheap; so only do
		// keep-alives for a short period of time, as these
		// active connections lock the server into only serving
		// that user. If the user has this page open, we don't
		// want another switching user to be locked out for
		// minutes. 5 seconds is enough to let browser hit
		// favicon.ico and such.
		IdleTimeout: 5 * time.Second,
		ErrorLog:    logger.StdLogger(logger.WithPrefix(s.logf, "ipnserver: ")),
	}
	if err := hs.Serve(ln); err != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return err
	}
	return nil
}

// ServeHTMLStatus serves an HTML status page at http://localhost:41112/ for
// Windows and via $DEBUG_LISTENER/debug/ipn when tailscaled's --debug flag
// is used to run a debug server.
func (s *Server) ServeHTMLStatus(w http.ResponseWriter, r *http.Request) {
	lb := s.lb.Load()
	if lb == nil {
		http.Error(w, "no LocalBackend", http.StatusServiceUnavailable)
		return
	}

	// As this is only meant for debug, verify there's no DNS name being used to
	// access this.
	if !strings.HasPrefix(r.Host, "localhost:") && strings.IndexFunc(r.Host, unicode.IsLetter) != -1 {
		http.Error(w, "invalid host", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Security-Policy", `default-src 'none'; frame-ancestors 'none'; script-src 'none'; script-src-elem 'none'; script-src-attr 'none'`)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	st := lb.Status()
	// TODO(bradfitz): add LogID and opts to st?
	st.WriteHTML(w)
}
