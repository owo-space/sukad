/*
 * V2Node ↔ OpenSnell multi-user bridge.
 *
 * Mirrors the structural pattern of core/mieru.go: a per-tag *snellNode
 * owns its own snell server, an atomic-snapshot user store, and a
 * traffic counter. addUsers / delUsers refresh the store dynamically;
 * the OpenSnell server's OnAuthorize hook does per-connection limit
 * checks and wraps the upstream conn for traffic accounting + rate
 * limiting.
 *
 * Wire-level compatibility with the official Surge snell-server is
 * preserved end-to-end — V2Node only adds *server-side* user
 * identification (trial-decrypt of the first AEAD header against each
 * registered PSK with a per-IP LRU cache). Clients do not need to
 * know anything has changed.
 */
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/missuo/opensnell/components/snell"
	panel "github.com/owo-space/sukad/api/sukashi"
	"github.com/owo-space/sukad/common/counter"
	"github.com/owo-space/sukad/common/format"
	localrate "github.com/owo-space/sukad/common/rate"
	"github.com/owo-space/sukad/limiter"
	log "github.com/sirupsen/logrus"
)

// logrusSlogHandler bridges OpenSnell's slog-based logger into V2Node's
// logrus pipeline so snell logs land in the same file / journald sink
// as every other inbound. Levels map straight through; structured
// attributes are flattened into logrus fields with no field-name
// rewrites (OpenSnell already uses sensible keys like "remote",
// "target", "user").
type logrusSlogHandler struct {
	tag   string
	attrs []slog.Attr
	group string
}

func (h *logrusSlogHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	// Map slog level → logrus level and check both — OpenSnell emits a
	// LOT of Debug() lines on the data path, so we drop those unless
	// logrus is also at Debug.
	switch {
	case lvl >= slog.LevelError:
		return log.IsLevelEnabled(log.ErrorLevel)
	case lvl >= slog.LevelWarn:
		return log.IsLevelEnabled(log.WarnLevel)
	case lvl >= slog.LevelInfo:
		return log.IsLevelEnabled(log.InfoLevel)
	default:
		return log.IsLevelEnabled(log.DebugLevel)
	}
}

func (h *logrusSlogHandler) Handle(_ context.Context, r slog.Record) error {
	fields := log.Fields{"component": h.tag}
	for _, a := range h.attrs {
		fields[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	entry := log.WithFields(fields)
	switch {
	case r.Level >= slog.LevelError:
		entry.Error(r.Message)
	case r.Level >= slog.LevelWarn:
		entry.Warn(r.Message)
	case r.Level >= slog.LevelInfo:
		entry.Info(r.Message)
	default:
		entry.Debug(r.Message)
	}
	return nil
}

func (h *logrusSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &logrusSlogHandler{tag: h.tag, attrs: merged, group: h.group}
}

func (h *logrusSlogHandler) WithGroup(name string) slog.Handler {
	// Group nesting is rare in OpenSnell's log calls; we just track
	// the most recent one and prefix attr keys with it. Simpler than
	// maintaining a full group stack and good enough in practice.
	return &logrusSlogHandler{tag: h.tag, attrs: h.attrs, group: name}
}

func slogFromLogrus(tag string) *slog.Logger {
	return slog.New(&logrusSlogHandler{tag: tag})
}

type snellNode struct {
	tag  string
	info *panel.NodeInfo

	store   *snell.StaticUserStore
	server  *snell.Server
	traffic *counter.TrafficCounter

	mu      sync.RWMutex
	users   map[string]panel.UserInfo
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func newSnellNode(tag string, info *panel.NodeInfo) *snellNode {
	return &snellNode{
		tag:     tag,
		info:    info,
		store:   snell.NewStaticUserStore(nil),
		traffic: counter.NewTrafficCounter(),
		users:   make(map[string]panel.UserInfo),
	}
}

// addUsers merges a freshly-pulled user set into the in-memory map, then
// either kicks off the snell server (first time we have at least one
// user) or just refreshes the user store snapshot atomically.
func (n *snellNode) addUsers(users []panel.UserInfo) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, u := range users {
		n.users[u.Uuid] = u
	}
	n.store.SetUsers(n.snellUsersLocked())
	if !n.started {
		if err := n.startLocked(); err != nil {
			return 0, err
		}
	}
	return len(users), nil
}

// delUsers removes the listed users from both the user store (so new
// connections auth-fail immediately) and the traffic counter (so we
// don't keep counting bytes against a user the panel has dropped). It
// does NOT proactively close in-flight TCP relays for those users —
// they'll terminate naturally on the next idle timeout, or sooner if
// the next OnAuthorize check sees them missing from the limiter map.
func (n *snellNode) delUsers(users []panel.UserInfo) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, u := range users {
		delete(n.users, u.Uuid)
		n.traffic.Delete(u.Uuid)
	}
	n.store.SetUsers(n.snellUsersLocked())
	return nil
}

// snellUsersLocked snapshots the current user map into the []snell.User
// shape the UserStore expects. Caller must hold n.mu.
func (n *snellNode) snellUsersLocked() []snell.User {
	out := make([]snell.User, 0, len(n.users))
	for uuid := range n.users {
		out = append(out, snell.User{
			Name: uuid,
			PSK:  []byte(uuid),
		})
	}
	return out
}

// getUserTrafficSlice drains the traffic counter into the panel-report
// shape, applying the configured min-traffic threshold to avoid pushing
// noise on idle clients. Mirrors mieruNode.getUserTrafficSlice.
func (n *snellNode) getUserTrafficSlice(mintraffic int) []panel.UserTraffic {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]panel.UserTraffic, 0)
	n.traffic.Counters.Range(func(key, value interface{}) bool {
		uuid := key.(string)
		t := value.(*counter.TrafficStorage)
		up := t.UpCounter.Load()
		down := t.DownCounter.Load()
		if up+down <= int64(mintraffic*1000) {
			return true
		}
		u, ok := n.users[uuid]
		if !ok {
			n.traffic.Delete(uuid)
			return true
		}
		t.UpCounter.Store(0)
		t.DownCounter.Store(0)
		out = append(out, panel.UserTraffic{
			UID:      u.Id,
			Upload:   up,
			Download: down,
		})
		return true
	})
	return out
}

func (n *snellNode) close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.started {
		return nil
	}
	if n.cancel != nil {
		n.cancel()
	}
	// Wait briefly for the listener goroutine to release the port —
	// otherwise a quick remove+add of the same node would fail to
	// re-bind.
	if n.done != nil {
		select {
		case <-n.done:
		case <-time.After(3 * time.Second):
			log.WithField("tag", n.tag).Warn("snell listener didn't exit within 3s of cancel")
		}
	}
	n.started = false
	n.server = nil
	n.cancel = nil
	n.done = nil
	return nil
}

// startLocked binds the snell listener and kicks off the accept loop.
// Caller must hold n.mu and the user store must already have at least
// one user — starting an empty server would refuse every connection,
// which is pointless.
func (n *snellNode) startLocked() error {
	if n.info.Common == nil {
		return errors.New("snell node missing common config")
	}
	listenIP := n.info.Common.ListenIP
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	if n.info.Common.ServerPort == 0 {
		return errors.New("snell node missing server_port")
	}

	ss := n.info.Common.SnellSettings
	cfg := snell.ServerConfig{
		Listen:          net.JoinHostPort(listenIP, fmt.Sprintf("%d", n.info.Common.ServerPort)),
		UserStore:       n.store,
		ObfsMode:        normalizeObfs(ss.Obfs),
		UDP:             boolDefault(ss.UDP, true),
		QUIC:            boolDefault(ss.QUIC, true),
		IPv6:            boolDefault(ss.IPv6, true),
		TFO:             boolDefault(ss.TFO, true),
		EgressInterface: ss.EgressInterface,
		DNS:             ss.DNS,
		DialTimeout:     5 * time.Second,
		OnAuthorize:     n.onAuthorize,
	}

	srv, err := snell.NewServer(cfg, slogFromLogrus("snell-"+n.tag))
	if err != nil {
		return fmt.Errorf("snell new server: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.WithFields(log.Fields{"tag": n.tag, "err": err}).Error("snell server exited")
		}
	}()
	n.server = srv
	n.cancel = cancel
	n.done = done
	n.started = true
	log.WithFields(log.Fields{
		"tag":    n.tag,
		"listen": cfg.Listen,
		"users":  n.store.Len(),
		"obfs":   cfg.ObfsMode,
		"quic":   cfg.QUIC,
		"tfo":    cfg.TFO,
	}).Info("snell node started")
	return nil
}

// onAuthorize is the OpenSnell-side hook invoked per CONNECT request
// AFTER PSK trial decryption has matched a user. We use it to (a)
// enforce per-user device + speed limits, (b) dial the upstream target
// through the server's configured dialer, and (c) wrap the upstream
// conn so each Read/Write contributes to the user's traffic counters
// AND respects the dynamic rate limit.
func (n *snellNode) onAuthorize(ctx context.Context, info snell.ConnContext, dialer snell.ContextDialer) (net.Conn, error) {
	host := ""
	if info.RemoteAddr != nil {
		h, _, _ := net.SplitHostPort(info.RemoteAddr.String())
		if h == "" {
			h = info.RemoteAddr.String()
		}
		host = h
	}
	bucket, reject := n.checkLimit(info.User, host)
	if reject {
		return nil, fmt.Errorf("snell: user %s exceeded device or speed limit", info.User)
	}
	upstream, err := dialer.DialContext(ctx, "tcp", info.Target)
	if err != nil {
		return nil, err
	}
	return &meteredUpstreamConn{
		Conn:    upstream,
		user:    info.User,
		traffic: n.traffic,
		limiter: bucket,
	}, nil
}

func (n *snellNode) checkLimit(uuid, ip string) (*localrate.DynamicBucket, bool) {
	l, err := limiter.GetLimiter(n.tag)
	if err != nil {
		return nil, false
	}
	// noUDPsource = true because each snell TCP CONNECT counts as one
	// device — there's no per-packet origin tracking to worry about.
	return l.CheckLimit(format.UserTag(n.tag, uuid), ip, true)
}

// meteredUpstreamConn wraps an upstream net.Conn so that:
//   - bytes WRITTEN to upstream (client → target) are counted as Upload
//   - bytes READ from upstream (target → client) are counted as Download
//   - both directions go through the user's DynamicBucket when set, so
//     the panel-driven speed limit applies in real time.
type meteredUpstreamConn struct {
	net.Conn
	user    string
	traffic *counter.TrafficCounter
	limiter *localrate.DynamicBucket
}

func (c *meteredUpstreamConn) Read(p []byte) (int, error) {
	if c.limiter != nil {
		c.limiter.Get().Wait(int64(len(p)))
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.traffic.Rx(c.user, n)
	}
	return n, err
}

func (c *meteredUpstreamConn) Write(p []byte) (int, error) {
	if c.limiter != nil {
		c.limiter.Get().Wait(int64(len(p)))
	}
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.traffic.Tx(c.user, n)
	}
	return n, err
}

// boolDefault dereferences an *bool field from SnellSettings, falling
// back to defaultVal when the pointer is nil. Lets the panel
// distinguish "unset" (use V2Node default) from "set to false".
func boolDefault(b *bool, defaultVal bool) bool {
	if b == nil {
		return defaultVal
	}
	return *b
}

func normalizeObfs(s string) string {
	switch s {
	case "", "off", "none":
		return "off"
	case "http", "tls":
		return s
	default:
		// Unrecognized values fall back to "off" rather than failing
		// the whole node bring-up. Logging happens upstream once
		// OpenSnell rejects the config — which shouldn't happen
		// since we normalize here, but the safety net is cheap.
		return "off"
	}
}
