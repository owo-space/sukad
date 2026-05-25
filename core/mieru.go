package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	mapi "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/constant"
	"github.com/enfein/mieru/v3/apis/model"
	"github.com/enfein/mieru/v3/apis/trafficpattern"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	mcommon "github.com/enfein/mieru/v3/pkg/common"
	"github.com/enfein/mieru/v3/pkg/protocol"
	"github.com/enfein/mieru/v3/pkg/socks5"
	mstderror "github.com/enfein/mieru/v3/pkg/stderror"
	panel "github.com/missuo/sukad/api/sukashi"
	"github.com/missuo/sukad/common/counter"
	"github.com/missuo/sukad/common/format"
	localrate "github.com/missuo/sukad/common/rate"
	"github.com/missuo/sukad/limiter"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type mieruNode struct {
	tag     string
	info    *panel.NodeInfo
	mux     *protocol.Mux
	traffic *counter.TrafficCounter

	mu      sync.RWMutex
	users   map[string]panel.UserInfo
	started bool
}

type meteredConn struct {
	net.Conn
	user    string
	traffic *counter.TrafficCounter
	limiter *localrate.DynamicBucket
}

func newMieruNode(tag string, info *panel.NodeInfo) *mieruNode {
	return &mieruNode{
		tag:     tag,
		info:    info,
		traffic: counter.NewTrafficCounter(),
		users:   make(map[string]panel.UserInfo),
	}
}

func (m *mieruNode) addUsers(users []panel.UserInfo) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, user := range users {
		m.users[user.Uuid] = user
	}
	if len(m.users) == 0 {
		return 0, nil
	}
	if m.started {
		m.mux.SetServerUsers(m.mieruUsersLocked())
		return len(users), nil
	}
	if err := m.startLocked(); err != nil {
		return 0, err
	}
	return len(users), nil
}

func (m *mieruNode) delUsers(users []panel.UserInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, user := range users {
		delete(m.users, user.Uuid)
		m.traffic.Delete(user.Uuid)
	}
	if m.started {
		m.mux.SetServerUsers(m.mieruUsersLocked())
	}
	return nil
}

func (m *mieruNode) getUserTrafficSlice(mintraffic int) []panel.UserTraffic {
	m.mu.RLock()
	defer m.mu.RUnlock()
	trafficSlice := make([]panel.UserTraffic, 0)
	m.traffic.Counters.Range(func(key, value interface{}) bool {
		uuid := key.(string)
		traffic := value.(*counter.TrafficStorage)
		up := traffic.UpCounter.Load()
		down := traffic.DownCounter.Load()
		if up+down <= int64(mintraffic*1000) {
			return true
		}
		user, ok := m.users[uuid]
		if !ok {
			m.traffic.Delete(uuid)
			return true
		}
		traffic.UpCounter.Store(0)
		traffic.DownCounter.Store(0)
		trafficSlice = append(trafficSlice, panel.UserTraffic{
			UID:      user.Id,
			Upload:   up,
			Download: down,
		})
		return true
	})
	return trafficSlice
}

func (m *mieruNode) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started || m.mux == nil {
		return nil
	}
	err := m.mux.Close()
	m.started = false
	m.mux = nil
	return err
}

func (m *mieruNode) startLocked() error {
	endpoints, err := m.endpointsLocked()
	if err != nil {
		return err
	}
	trafficPattern, err := mieruTrafficPattern(m.info.Common.MieruSettings.TrafficPattern)
	if err != nil {
		return err
	}
	mux := protocol.NewMux(false).
		SetTrafficPattern(trafficPattern).
		SetServerUsers(m.mieruUsersLocked()).
		SetServerUserHintIsMandatory(m.info.Common.MieruSettings.UserHintIsMandatory).
		SetEndpoints(endpoints)
	if err := mux.Start(); err != nil {
		return err
	}
	m.mux = mux
	m.started = true
	go m.acceptLoop(mux)
	log.WithField("tag", m.tag).Info("mieru node started")
	return nil
}

func (m *mieruNode) mieruUsersLocked() map[string]*appctlpb.User {
	users := make(map[string]*appctlpb.User, len(m.users))
	for _, user := range m.users {
		uuid := user.Uuid
		users[uuid] = &appctlpb.User{
			Name:            proto.String(uuid),
			Password:        proto.String(uuid),
			AllowPrivateIP:  proto.Bool(true),
			AllowLoopbackIP: proto.Bool(false),
		}
	}
	return users
}

func (m *mieruNode) endpointsLocked() ([]protocol.UnderlayProperties, error) {
	mtu := m.info.Common.MieruSettings.MTU
	if mtu == 0 {
		mtu = mcommon.DefaultMTU
	}
	listenIP := net.ParseIP(m.info.Common.ListenIP)
	if listenIP == nil {
		listenIP = net.ParseIP("0.0.0.0")
	}

	bindings := m.info.Common.MieruSettings.PortBindings
	if len(bindings) == 0 {
		protoName := strings.ToUpper(m.info.Common.Network)
		if protoName == "" {
			protoName = "TCP"
		}
		bindings = []panel.MieruPortBinding{{
			Port:     m.info.Common.ServerPort,
			Protocol: protoName,
		}}
	}

	var endpoints []protocol.UnderlayProperties
	for _, binding := range bindings {
		ports, err := expandMieruPorts(binding)
		if err != nil {
			return nil, err
		}
		transport, err := mieruTransport(binding.Protocol)
		if err != nil {
			return nil, err
		}
		for _, port := range ports {
			switch transport {
			case mcommon.StreamTransport:
				endpoints = append(endpoints, protocol.NewUnderlayProperties(
					mtu,
					transport,
					&net.TCPAddr{IP: listenIP, Port: port},
					nil,
				))
			case mcommon.PacketTransport:
				endpoints = append(endpoints, protocol.NewUnderlayProperties(
					mtu,
					transport,
					&net.UDPAddr{IP: listenIP, Port: port},
					nil,
				))
			default:
				return nil, fmt.Errorf("unsupported mieru transport: %v", transport)
			}
		}
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("mieru has no listening endpoint")
	}
	return endpoints, nil
}

func expandMieruPorts(binding panel.MieruPortBinding) ([]int, error) {
	if binding.Port != 0 {
		if binding.Port < 1 || binding.Port > 65535 {
			return nil, fmt.Errorf("mieru port %d is invalid", binding.Port)
		}
		return []int{binding.Port}, nil
	}
	portRange := binding.PortRange
	if portRange == "" {
		portRange = binding.PortRangeAlt
	}
	parts := strings.Split(portRange, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("mieru port binding must set port or port range")
	}
	from, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("parse mieru port range %q: %w", portRange, err)
	}
	to, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("parse mieru port range %q: %w", portRange, err)
	}
	if from < 1 || to > 65535 || from > to {
		return nil, fmt.Errorf("mieru port range %q is invalid", portRange)
	}
	ports := make([]int, 0, to-from+1)
	for port := from; port <= to; port++ {
		ports = append(ports, port)
	}
	return ports, nil
}

func mieruTransport(protocolName string) (mcommon.TransportProtocol, error) {
	switch strings.ToUpper(protocolName) {
	case "", "TCP":
		return mcommon.StreamTransport, nil
	case "UDP":
		return mcommon.PacketTransport, nil
	default:
		return 0, fmt.Errorf("unsupported mieru transport protocol: %s", protocolName)
	}
}

func mieruTrafficPattern(raw json.RawMessage) (*trafficpattern.Config, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return trafficpattern.NewConfig(nil)
	}
	pattern := &appctlpb.TrafficPattern{}
	if err := protojson.Unmarshal(raw, pattern); err != nil {
		return nil, fmt.Errorf("unmarshal mieru traffic pattern: %w", err)
	}
	return trafficpattern.NewConfig(pattern)
}

func (m *mieruNode) acceptLoop(mux *protocol.Mux) {
	for {
		conn, err := mux.Accept()
		if err != nil {
			if errors.Is(err, io.EOF) || mstderror.IsClosed(err) {
				return
			}
			log.WithFields(log.Fields{"tag": m.tag, "err": err}).Debug("mieru accept failed")
			return
		}
		go m.handleConn(conn)
	}
}

func (m *mieruNode) handleConn(conn net.Conn) {
	defer conn.Close()

	userCtx, ok := conn.(mapi.UserContext)
	if !ok {
		log.WithField("tag", m.tag).Debug("mieru connection has no user context")
		return
	}
	uuid := userCtx.UserName()
	if uuid == "" {
		log.WithField("tag", m.tag).Debug("mieru connection has empty user name")
		return
	}

	bucket, reject := m.checkLimit(uuid, conn.RemoteAddr())
	metered := &meteredConn{
		Conn:    conn,
		user:    uuid,
		traffic: m.traffic,
		limiter: bucket,
	}
	if reject {
		_ = writeSocks5Reply(metered, constant.Socks5ReplyNotAllowedByRuleSet, nil)
		return
	}

	if err := metered.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		log.WithFields(log.Fields{"tag": m.tag, "err": err}).Debug("set mieru read deadline failed")
	}
	req := &model.Request{}
	err := req.ReadFromSocks5(metered)
	_ = metered.SetReadDeadline(time.Time{})
	if err != nil {
		log.WithFields(log.Fields{"tag": m.tag, "err": err}).Debug("read mieru socks request failed")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch req.Command {
	case constant.Socks5ConnectCmd:
		err = m.handleConnect(ctx, req, metered)
	case constant.Socks5UDPAssociateCmd:
		err = m.handleUDPAssociate(ctx, metered)
	default:
		_ = writeSocks5Reply(metered, constant.Socks5ReplyCommandNotSupported, nil)
		err = fmt.Errorf("unsupported socks command: %d", req.Command)
	}
	if err != nil && !mstderror.IsEOF(err) && !mstderror.IsClosed(err) {
		log.WithFields(log.Fields{
			"tag":  m.tag,
			"user": uuid,
			"err":  err,
		}).Debug("mieru connection closed with error")
	}
}

func (m *mieruNode) checkLimit(uuid string, addr net.Addr) (*localrate.DynamicBucket, bool) {
	l, err := limiter.GetLimiter(m.tag)
	if err != nil {
		return nil, false
	}
	host := ""
	if addr != nil {
		host, _, _ = net.SplitHostPort(addr.String())
		if host == "" {
			host = addr.String()
		}
	}
	return l.CheckLimit(format.UserTag(m.tag, uuid), host, true)
}

func (m *mieruNode) handleConnect(ctx context.Context, req *model.Request, conn net.Conn) error {
	var dialer net.Dialer
	target, err := dialer.DialContext(ctx, "tcp", req.DstAddr.String())
	if err != nil {
		reply := constant.Socks5ReplyHostUnreachable
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "refused") {
			reply = constant.Socks5ReplyConnectionRefused
		} else if strings.Contains(msg, "network is unreachable") {
			reply = constant.Socks5ReplyNetworkUnreachable
		}
		_ = writeSocks5Reply(conn, reply, nil)
		return fmt.Errorf("connect to %s failed: %w", req.DstAddr.String(), err)
	}
	defer target.Close()

	var bind *net.TCPAddr
	if local, ok := target.LocalAddr().(*net.TCPAddr); ok {
		bind = local
	}
	if err := writeSocks5Reply(conn, constant.Socks5ReplySuccess, bind); err != nil {
		return err
	}
	return bidiCopy(conn, target)
}

func (m *mieruNode) handleUDPAssociate(ctx context.Context, conn net.Conn) error {
	udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort("0.0.0.0", "0"))
	if err != nil {
		_ = writeSocks5Reply(conn, constant.Socks5ReplyServerFailure, nil)
		return err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = writeSocks5Reply(conn, constant.Socks5ReplyServerFailure, nil)
		return err
	}

	_, portStr, err := net.SplitHostPort(udpConn.LocalAddr().String())
	if err != nil {
		udpConn.Close()
		_ = writeSocks5Reply(conn, constant.Socks5ReplyServerFailure, nil)
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		udpConn.Close()
		_ = writeSocks5Reply(conn, constant.Socks5ReplyServerFailure, nil)
		return err
	}
	if err := writeSocks5Reply(conn, constant.Socks5ReplySuccess, &net.TCPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: port,
	}); err != nil {
		udpConn.Close()
		return err
	}
	return socks5.RunUDPAssociateLoop(udpConn, mapi.NewPacketOverStreamTunnel(conn), net.DefaultResolver)
}

func writeSocks5Reply(conn net.Conn, reply byte, bind *net.TCPAddr) error {
	addr := model.AddrSpec{IP: net.IPv4(0, 0, 0, 0)}
	if bind != nil {
		addr.IP = bind.IP
		addr.Port = bind.Port
	}
	resp := &model.Response{
		Reply:    reply,
		BindAddr: addr,
	}
	return resp.WriteToSocks5(conn)
}

func bidiCopy(conn1, conn2 net.Conn) error {
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn1, conn2)
		conn1.Close()
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(conn2, conn1)
		conn2.Close()
		errCh <- err
	}()
	err := <-errCh
	<-errCh
	return err
}

func (c *meteredConn) Read(p []byte) (int, error) {
	if c.limiter != nil {
		c.limiter.Get().Wait(int64(len(p)))
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.traffic.Tx(c.user, n)
	}
	return n, err
}

func (c *meteredConn) Write(p []byte) (int, error) {
	if c.limiter != nil {
		c.limiter.Get().Wait(int64(len(p)))
	}
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.traffic.Rx(c.user, n)
	}
	return n, err
}
