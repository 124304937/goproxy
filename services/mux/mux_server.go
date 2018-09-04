package mux

import (
	"crypto/tls"
	"fmt"
	"io"
	logger "log"
	"math/rand"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"bitbucket.org/snail/proxy/services"
	"bitbucket.org/snail/proxy/services/kcpcfg"
	"bitbucket.org/snail/proxy/utils"
	"bitbucket.org/snail/proxy/utils/jumper"
	"bitbucket.org/snail/proxy/utils/mapx"

	"github.com/golang/snappy"
	//"github.com/xtaci/smux"
	smux "github.com/hashicorp/yamux"
)

const (
	CONN_CLIENT_CONTROL = uint8(1)
	CONN_SERVER         = uint8(4)
	CONN_CLIENT         = uint8(5)
)

type MuxServerArgs struct {
	Parent       *string
	ParentType   *string
	CertFile     *string
	KeyFile      *string
	CertBytes    []byte
	KeyBytes     []byte
	Local        *string
	IsUDP        *bool
	Key          *string
	Remote       *string
	Timeout      *int
	Route        *[]string
	Mgr          *MuxServerManager
	IsCompress   *bool
	SessionCount *int
	KCP          kcpcfg.KCPConfigArgs
	Jumper       *string
}
type MuxServer struct {
	cfg      MuxServerArgs
	sc       utils.ServerChannel
	sessions mapx.ConcurrentMap
	lockChn  chan bool
	isStop   bool
	log      *logger.Logger
	jumper   *jumper.Jumper
	udpConns mapx.ConcurrentMap
}

type MuxServerManager struct {
	cfg      MuxServerArgs
	serverID string
	servers  []*services.Service
	log      *logger.Logger
}

func NewMuxServerManager() services.Service {
	return &MuxServerManager{
		cfg:      MuxServerArgs{},
		serverID: utils.Uniqueid(),
		servers:  []*services.Service{},
	}
}

func (s *MuxServerManager) Start(args interface{}, log *logger.Logger) (err error) {
	s.log = log
	s.cfg = args.(MuxServerArgs)
	if err = s.CheckArgs(); err != nil {
		return
	}
	if *s.cfg.Parent != "" {
		s.log.Printf("use %s parent %s", *s.cfg.ParentType, *s.cfg.Parent)
	} else {
		err = fmt.Errorf("parent required")
		return
	}

	if err = s.InitService(); err != nil {
		return
	}

	s.log.Printf("server id: %s", s.serverID)
	//s.log.Printf("route:%v", *s.cfg.Route)
	for _, _info := range *s.cfg.Route {
		if _info == "" {
			continue
		}
		IsUDP := *s.cfg.IsUDP
		if strings.HasPrefix(_info, "udp://") {
			IsUDP = true
		}
		info := strings.TrimPrefix(_info, "udp://")
		info = strings.TrimPrefix(info, "tcp://")
		_routeInfo := strings.Split(info, "@")
		server := NewMuxServer()

		local := _routeInfo[0]
		remote := _routeInfo[1]
		KEY := *s.cfg.Key
		if strings.HasPrefix(remote, "[") {
			KEY = remote[1:strings.LastIndex(remote, "]")]
			remote = remote[strings.LastIndex(remote, "]")+1:]
		}
		if strings.HasPrefix(remote, ":") {
			remote = fmt.Sprintf("127.0.0.1%s", remote)
		}
		err = server.Start(MuxServerArgs{
			CertBytes:    s.cfg.CertBytes,
			KeyBytes:     s.cfg.KeyBytes,
			Parent:       s.cfg.Parent,
			CertFile:     s.cfg.CertFile,
			KeyFile:      s.cfg.KeyFile,
			Local:        &local,
			IsUDP:        &IsUDP,
			Remote:       &remote,
			Key:          &KEY,
			Timeout:      s.cfg.Timeout,
			Mgr:          s,
			IsCompress:   s.cfg.IsCompress,
			SessionCount: s.cfg.SessionCount,
			KCP:          s.cfg.KCP,
			ParentType:   s.cfg.ParentType,
			Jumper:       s.cfg.Jumper,
		}, log)

		if err != nil {
			return
		}
		s.servers = append(s.servers, &server)
	}
	return
}
func (s *MuxServerManager) Clean() {
	s.StopService()
}
func (s *MuxServerManager) StopService() {
	for _, server := range s.servers {
		(*server).Clean()
	}
}
func (s *MuxServerManager) CheckArgs() (err error) {
	if *s.cfg.CertFile == "" || *s.cfg.KeyFile == "" {
		err = fmt.Errorf("cert and key file required")
		return
	}
	if *s.cfg.ParentType == "tls" {
		s.cfg.CertBytes, s.cfg.KeyBytes, err = utils.TlsBytes(*s.cfg.CertFile, *s.cfg.KeyFile)
		if err != nil {
			return
		}
	}
	return
}
func (s *MuxServerManager) InitService() (err error) {
	return
}

func NewMuxServer() services.Service {
	return &MuxServer{
		cfg:      MuxServerArgs{},
		lockChn:  make(chan bool, 1),
		sessions: mapx.NewConcurrentMap(),
		isStop:   false,
	}
}

type MuxUDPPacketItem struct {
	packet    *[]byte
	localAddr *net.UDPAddr
	srcAddr   *net.UDPAddr
}
type UDPConnItem struct {
	conn      *net.Conn
	touchtime int64
	srcAddr   *net.UDPAddr
	localAddr *net.UDPAddr
	connid    string
}

func (s *MuxServer) StopService() {
	defer func() {
		e := recover()
		if e != nil {
			s.log.Printf("stop server service crashed,%s", e)
		} else {
			s.log.Printf("service server stoped")
		}
	}()
	s.isStop = true
	for _, sess := range s.sessions.Items() {
		sess.(*smux.Session).Close()
	}
	if s.sc.Listener != nil {
		(*s.sc.Listener).Close()
	}
	if s.sc.UDPListener != nil {
		(*s.sc.UDPListener).Close()
	}
}
func (s *MuxServer) InitService() (err error) {
	s.UDPGCDeamon()
	return
}
func (s *MuxServer) CheckArgs() (err error) {
	if *s.cfg.Remote == "" {
		err = fmt.Errorf("remote required")
		return
	}
	if *s.cfg.Jumper != "" {
		if *s.cfg.ParentType != "tls" && *s.cfg.ParentType != "tcp" {
			err = fmt.Errorf("jumper only worked of -T is tls or tcp")
			return
		}
		var j jumper.Jumper
		j, err = jumper.New(*s.cfg.Jumper, time.Millisecond*time.Duration(*s.cfg.Timeout))
		if err != nil {
			err = fmt.Errorf("parse jumper fail, err %s", err)
			return
		}
		s.jumper = &j
	}
	return
}

func (s *MuxServer) Start(args interface{}, log *logger.Logger) (err error) {
	s.log = log
	s.cfg = args.(MuxServerArgs)
	if err = s.CheckArgs(); err != nil {
		return
	}
	if err = s.InitService(); err != nil {
		return
	}
	host, port, _ := net.SplitHostPort(*s.cfg.Local)
	p, _ := strconv.Atoi(port)
	s.sc = utils.NewServerChannel(host, p, s.log)
	if *s.cfg.IsUDP {
		err = s.sc.ListenUDP(func(listener *net.UDPConn, packet []byte, localAddr, srcAddr *net.UDPAddr) {
			s.UDPSend(packet, localAddr, srcAddr)
		})
		if err != nil {
			return
		}
		s.log.Printf("server on %s", (*s.sc.UDPListener).LocalAddr())
	} else {
		err = s.sc.ListenTCP(func(inConn net.Conn) {
			defer func() {
				if err := recover(); err != nil {
					s.log.Printf("connection handler crashed with err : %s \nstack: %s", err, string(debug.Stack()))
				}
			}()
			var outConn net.Conn
			var ID string
			for {
				if s.isStop {
					return
				}
				outConn, ID, err = s.GetOutConn()
				if err != nil {
					utils.CloseConn(&outConn)
					s.log.Printf("connect to %s fail, err: %s, retrying...", *s.cfg.Parent, err)
					time.Sleep(time.Second * 3)
					continue
				} else {
					break
				}
			}
			s.log.Printf("%s stream %s created", *s.cfg.Key, ID)
			if *s.cfg.IsCompress {
				die1 := make(chan bool, 1)
				die2 := make(chan bool, 1)
				go func() {
					io.Copy(inConn, snappy.NewReader(outConn))
					die1 <- true
				}()
				go func() {
					io.Copy(snappy.NewWriter(outConn), inConn)
					die2 <- true
				}()
				select {
				case <-die1:
				case <-die2:
				}
				outConn.Close()
				inConn.Close()
				s.log.Printf("%s stream %s released", *s.cfg.Key, ID)
			} else {
				utils.IoBind(inConn, outConn, func(err interface{}) {
					s.log.Printf("%s stream %s released", *s.cfg.Key, ID)
				}, s.log)
			}
		})
		if err != nil {
			return
		}
		s.log.Printf("server on %s", (*s.sc.Listener).Addr())
	}
	return
}
func (s *MuxServer) Clean() {
	s.StopService()
}
func (s *MuxServer) GetOutConn() (outConn net.Conn, ID string, err error) {
	i := 1
	if *s.cfg.SessionCount > 0 {
		i = rand.Intn(*s.cfg.SessionCount)
	}
	outConn, err = s.GetConn(fmt.Sprintf("%d", i))
	if err != nil {
		if !strings.Contains(err.Error(), "can not connect at same time") {
			s.log.Printf("connection err: %s", err)
		}
		return
	}
	remoteAddr := "tcp:" + *s.cfg.Remote
	if *s.cfg.IsUDP {
		remoteAddr = "udp:" + *s.cfg.Remote
	}
	ID = utils.Uniqueid()
	outConn.SetDeadline(time.Now().Add(time.Millisecond * time.Duration(*s.cfg.Timeout)))
	_, err = outConn.Write(utils.BuildPacketData(ID, remoteAddr, s.cfg.Mgr.serverID))
	outConn.SetDeadline(time.Time{})
	if err != nil {
		s.log.Printf("write stream data err: %s ,retrying...", err)
		utils.CloseConn(&outConn)
		return
	}
	return
}
func (s *MuxServer) GetConn(index string) (conn net.Conn, err error) {
	select {
	case s.lockChn <- true:
	default:
		err = fmt.Errorf("can not connect at same time")
		return
	}
	defer func() {
		<-s.lockChn
	}()
	var session *smux.Session
	_session, ok := s.sessions.Get(index)
	if !ok {
		var c net.Conn
		c, err = s.getParentConn()
		if err != nil {
			return
		}
		c.SetDeadline(time.Now().Add(time.Millisecond * time.Duration(*s.cfg.Timeout)))
		_, err = c.Write(utils.BuildPacket(CONN_SERVER, *s.cfg.Key, s.cfg.Mgr.serverID))
		c.SetDeadline(time.Time{})
		if err != nil {
			c.Close()
			return
		}
		if err == nil {
			session, err = smux.Client(c, nil)
			if err != nil {
				return
			}
		}
		if _sess, ok := s.sessions.Get(index); ok {
			_sess.(*smux.Session).Close()
		}
		s.sessions.Set(index, session)
		s.log.Printf("session[%s] created", index)
		go func() {
			for {
				if s.isStop {
					return
				}
				if session.IsClosed() {
					s.sessions.Remove(index)
					break
				}
				time.Sleep(time.Second * 5)
			}
		}()
	} else {
		session = _session.(*smux.Session)
	}
	conn, err = session.OpenStream()
	if err != nil {
		session.Close()
		s.sessions.Remove(index)
	}
	return
}
func (s *MuxServer) getParentConn() (conn net.Conn, err error) {
	if *s.cfg.ParentType == "tls" {
		if s.jumper == nil {
			var _conn tls.Conn
			_conn, err = utils.TlsConnectHost(*s.cfg.Parent, *s.cfg.Timeout, s.cfg.CertBytes, s.cfg.KeyBytes, nil)
			if err == nil {
				conn = net.Conn(&_conn)
			}
		} else {
			conf, err := utils.TlsConfig(s.cfg.CertBytes, s.cfg.KeyBytes, nil)
			if err != nil {
				return nil, err
			}
			var _c net.Conn
			_c, err = s.jumper.Dial(*s.cfg.Parent, time.Millisecond*time.Duration(*s.cfg.Timeout))
			if err == nil {
				conn = net.Conn(tls.Client(_c, conf))
			}
		}

	} else if *s.cfg.ParentType == "kcp" {
		conn, err = utils.ConnectKCPHost(*s.cfg.Parent, s.cfg.KCP)
	} else {
		if s.jumper == nil {
			conn, err = utils.ConnectHost(*s.cfg.Parent, *s.cfg.Timeout)
		} else {
			conn, err = s.jumper.Dial(*s.cfg.Parent, time.Millisecond*time.Duration(*s.cfg.Timeout))
		}
	}
	return
}
func (s *MuxServer) UDPGCDeamon() {
	gctime := int64(60)
	go func() {
		if s.isStop {
			return
		}
		timer := time.NewTicker(time.Second)
		for {
			<-timer.C
			gcKeys := []string{}
			s.udpConns.IterCb(func(key string, v interface{}) {
				if time.Now().Unix()-v.(*UDPConnItem).touchtime > gctime {
					(*(v.(*UDPConnItem).conn)).Close()
					gcKeys = append(gcKeys, key)
					s.log.Printf("gc udp conn %s", v.(*UDPConnItem).connid)
				}
			})
			for _, k := range gcKeys {
				s.udpConns.Remove(k)
			}
			gcKeys = nil
		}
	}()
}
func (s *MuxServer) UDPSend(data []byte, localAddr, srcAddr *net.UDPAddr) {
	var (
		uc      *UDPConnItem
		key     = srcAddr.String()
		ID      string
		err     error
		outconn net.Conn
	)
	v, ok := s.udpConns.Get(key)
	if !ok {
		for {
			outconn, ID, err = s.GetOutConn()
			if err != nil && strings.Contains(err.Error(), "can not connect at same time") {
				time.Sleep(time.Millisecond * 500)
				continue
			} else {
				break
			}
		}
		if err != nil {
			s.log.Printf("connect to %s fail, err: %s", *s.cfg.Parent, err)
			return
		}
		uc = &UDPConnItem{
			conn:      &outconn,
			srcAddr:   srcAddr,
			localAddr: localAddr,
			connid:    ID,
		}
		s.udpConns.Set(key, uc)
		s.UDPRevecive(key, ID)
	} else {
		uc = v.(*UDPConnItem)
	}
	go func() {
		defer func() {
			if e := recover(); e != nil {
				(*uc.conn).Close()
				s.udpConns.Remove(key)
				s.log.Printf("udp sender crashed with error : %s", e)
			}
		}()
		uc.touchtime = time.Now().Unix()
		(*uc.conn).SetWriteDeadline(time.Now().Add(time.Millisecond * time.Duration(*s.cfg.Timeout)))
		_, err = (*uc.conn).Write(utils.UDPPacket(srcAddr.String(), data))
		(*uc.conn).SetWriteDeadline(time.Time{})
		if err != nil {
			s.log.Printf("write udp packet to %s fail ,flush err:%s ", *s.cfg.Parent, err)
		}
	}()
}
func (s *MuxServer) UDPRevecive(key, ID string) {
	go func() {
		s.log.Printf("udp conn %s connected", ID)
		var uc *UDPConnItem
		defer func() {
			if uc != nil {
				(*uc.conn).Close()
			}
			s.udpConns.Remove(key)
			s.log.Printf("udp conn %s released", ID)
		}()
		v, ok := s.udpConns.Get(key)
		if !ok {
			s.log.Printf("[warn] udp conn not exists for %s, connid : %s", key, ID)
			return
		}
		uc = v.(*UDPConnItem)
		for {
			_, body, err := utils.ReadUDPPacket(*uc.conn)
			if err != nil {
				if strings.Contains(err.Error(), "n != int(") {
					continue
				}
				if err != io.EOF {
					s.log.Printf("udp conn read udp packet fail , err: %s ", err)
				}
				return
			}
			uc.touchtime = time.Now().Unix()
			go s.sc.UDPListener.WriteToUDP(body, uc.srcAddr)
		}
	}()
}
