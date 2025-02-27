package main

import (
	crypto_rand "crypto/rand"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"io"
	"io/ioutil"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/cloudflare/circl/dh/x25519"
	"github.com/jedisct1/dlog"
	clocksmith "github.com/jedisct1/go-clocksmith"
	stamps "github.com/jedisct1/go-dnsstamps"
)

// This is the required size of the OOB buffer to pass to ReadMsgUDP.
var udpOOBSize = func() int {
	// We can't know whether we'll get an IPv4 control message or an
	// IPv6 control message ahead of time. To get around this, we size
	// the buffer equal to the largest of the two.

	oob4 := ipv4.NewControlMessage(ipv4.FlagDst | ipv4.FlagInterface)
	oob6 := ipv6.NewControlMessage(ipv6.FlagDst | ipv6.FlagInterface)

	if len(oob4) > len(oob6) {
		return len(oob4)
	}

	return len(oob6)
}()

type Proxy struct {
	userName                     string
	child                        bool
	proxyPublicKey               [32]byte
	proxySecretKey               [32]byte
	ephemeralKeys                bool
	questionSizeEstimator        QuestionSizeEstimator
	serversInfo                  ServersInfo
	timeout                      time.Duration
	certRefreshDelay             time.Duration
	certRefreshDelayAfterFailure time.Duration
	certIgnoreTimestamp          bool
	mainProto                    string
	listenAddresses              []string
	daemonize                    bool
	registeredServers            []RegisteredServer
	pluginBlockIPv6              bool
	cache                        bool
	cacheSize                    int
	cacheNegMinTTL               uint32
	cacheNegMaxTTL               uint32
	cacheMinTTL                  uint32
	cacheMaxTTL                  uint32
	queryLogFile                 string
	queryLogFormat               string
	queryLogIgnoredQtypes        []string
	nxLogFile                    string
	nxLogFormat                  string
	blockNameFile                string
	whitelistNameFile            string
	blockNameLogFile             string
	whitelistNameLogFile         string
	blockNameFormat              string
	whitelistNameFormat          string
	blockIPFile                  string
	blockIPLogFile               string
	blockIPFormat                string
	forwardFile                  string
	cloakFile                    string
	pluginsGlobals               PluginsGlobals
	urlsToPrefetch               []URLToPrefetch
	clientsCount                 uint32
	maxClients                   uint32
	xTransport                   *XTransport
	allWeeklyRanges              *map[string]WeeklyRanges
	logMaxSize                   int
	logMaxAge                    int
	logMaxBackups                int
	refusedCodeInResponses       bool
	showCerts                    bool
}

func (proxy *Proxy) StartProxy() {
	proxy.questionSizeEstimator = NewQuestionSizeEstimator()
	if _, err := crypto_rand.Read(proxy.proxySecretKey[:]); err != nil {
		dlog.Fatal(err)
	}

	var cfProxyPublicKey, cfProxySecretKey x25519.Key
	copy(cfProxySecretKey[:], proxy.proxySecretKey[:])
	x25519.KeyGen(&cfProxyPublicKey, &cfProxySecretKey)
	copy(proxy.proxyPublicKey[:], cfProxyPublicKey[:])

	for _, registeredServer := range proxy.registeredServers {
		proxy.serversInfo.registerServer(proxy, registeredServer.name, registeredServer.stamp)
	}

	for _, listenAddrStr := range proxy.listenAddresses {
		listenUDPAddr, err := net.ResolveUDPAddr("udp", listenAddrStr)
		if err != nil {
			dlog.Fatal(err)
		}
		listenTCPAddr, err := net.ResolveTCPAddr("tcp", listenAddrStr)
		if err != nil {
			dlog.Fatal(err)
		}

		// if 'userName' is not set, continue as before
		if !(len(proxy.userName) > 0) {
			if err := proxy.udpListenerFromAddr(listenUDPAddr); err != nil {
				dlog.Fatal(err)
			}
			if err := proxy.tcpListenerFromAddr(listenTCPAddr); err != nil {
				dlog.Fatal(err)
			}
		} else {
			// if 'userName' is set and we are the parent process
			if !proxy.child {
				// parent
				listenerUDP, err := net.ListenUDP("udp", listenUDPAddr)
				if err != nil {
					dlog.Fatal(err)
				}
				listenerTCP, err := net.ListenTCP("tcp", listenTCPAddr)
				if err != nil {
					dlog.Fatal(err)
				}

				fdUDP, err := listenerUDP.File() // On Windows, the File method of UDPConn is not implemented.
				if err != nil {
					dlog.Fatal(err)
				}
				fdTCP, err := listenerTCP.File() // On Windows, the File method of TCPListener is not implemented.
				if err != nil {
					dlog.Fatal(err)
				}
				defer listenerUDP.Close()
				defer listenerTCP.Close()
				FileDescriptors = append(FileDescriptors, fdUDP)
				FileDescriptors = append(FileDescriptors, fdTCP)

				// if 'userName' is set and we are the child process
			} else {
				// child
				listenerUDP, err := net.FilePacketConn(os.NewFile(uintptr(3+FileDescriptorNum), "listenerUDP"))
				if err != nil {
					dlog.Fatal(err)
				}
				FileDescriptorNum++

				listenerTCP, err := net.FileListener(os.NewFile(uintptr(3+FileDescriptorNum), "listenerTCP"))
				if err != nil {
					dlog.Fatal(err)
				}
				FileDescriptorNum++

				dlog.Noticef("Now listening to %v [UDP]", listenUDPAddr)
				go proxy.udpListener(listenerUDP.(*net.UDPConn))

				dlog.Noticef("Now listening to %v [TCP]", listenAddrStr)
				go proxy.tcpListener(listenerTCP.(*net.TCPListener))
			}
		}
	}

	// if 'userName' is set and we are the parent process drop privilege and exit
	if len(proxy.userName) > 0 && !proxy.child {
		proxy.dropPrivilege(proxy.userName, FileDescriptors)
	}
	if err := proxy.SystemDListeners(); err != nil {
		dlog.Fatal(err)
	}
	liveServers, err := proxy.serversInfo.refresh(proxy)
	if proxy.showCerts {
		os.Exit(0)
	}
	if liveServers > 0 {
		dlog.Noticef("dnscrypt-proxy is ready - live servers: %d", liveServers)
		if !proxy.child {
			ServiceManagerReadyNotify()
		}
	} else if err != nil {
		dlog.Error(err)
		dlog.Notice("dnscrypt-proxy is waiting for at least one server to be reachable")
	}
	proxy.prefetcher(&proxy.urlsToPrefetch)
	if len(proxy.serversInfo.registeredServers) > 0 {
		go func() {
			for {
				delay := proxy.certRefreshDelay
				if proxy.serversInfo.liveServers() == 0 {
					delay = proxy.certRefreshDelayAfterFailure
				}
				clocksmith.Sleep(delay)
				proxy.serversInfo.refresh(proxy)
			}
		}()
	}
}

func (proxy *Proxy) prefetcher(urlsToPrefetch *[]URLToPrefetch) {
	go func() {
		for {
			now := time.Now()
			for i := range *urlsToPrefetch {
				urlToPrefetch := &(*urlsToPrefetch)[i]
				if now.After(urlToPrefetch.when) {
					dlog.Debugf("Prefetching [%s]", urlToPrefetch.url)
					if err := PrefetchSourceURL(proxy.xTransport, urlToPrefetch); err != nil {
						dlog.Debugf("Prefetching [%s] failed: %s", urlToPrefetch.url, err)
					} else {
						dlog.Debugf("Prefetching [%s] succeeded. Next refresh scheduled for %v", urlToPrefetch.url, urlToPrefetch.when)
					}
				}
			}
			clocksmith.Sleep(60 * time.Second)
		}
	}()
}

func (proxy *Proxy) udpListener(clientPc *net.UDPConn) {
	err := setUDPSocketOptions(clientPc)
	if err != nil {
		dlog.Fatal(err)
	}

	defer clientPc.Close()
	for {
		buffer := make([]byte, MaxDNSPacketSize-1)
		oob := make([]byte, udpOOBSize)
		length, oobn, _, clientAddr, err := clientPc.ReadMsgUDP(buffer, oob)
		if err != nil {
			return
		}
		packet := buffer[:length]
		raddr := interface{}(clientAddr).(net.Addr)
		go func() {
			start := time.Now()
			if !proxy.clientsCountInc() {
				dlog.Warnf("Too many connections (max=%d)", proxy.maxClients)
				return
			}
			defer proxy.clientsCountDec()
			proxy.processIncomingQuery(proxy.serversInfo.getOne(), "udp", proxy.mainProto, packet, oob[:oobn], &raddr, clientPc, start)
		}()
	}
}

func (proxy *Proxy) udpListenerFromAddr(listenAddr *net.UDPAddr) error {
	clientPc, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return err
	}
	dlog.Noticef("Now listening to %v [UDP]", listenAddr)
	go proxy.udpListener(clientPc)
	return nil
}

func (proxy *Proxy) tcpListener(acceptPc *net.TCPListener) {
	defer acceptPc.Close()
	for {
		clientPc, err := acceptPc.Accept()
		if err != nil {
			continue
		}
		go func() {
			start := time.Now()
			defer clientPc.Close()
			if !proxy.clientsCountInc() {
				dlog.Warnf("Too many connections (max=%d)", proxy.maxClients)
				return
			}
			defer proxy.clientsCountDec()
			clientPc.SetDeadline(time.Now().Add(proxy.timeout))
			packet, err := ReadPrefixed(&clientPc)
			if err != nil || len(packet) < MinDNSPacketSize {
				return
			}
			clientAddr := clientPc.RemoteAddr()
			proxy.processIncomingQuery(proxy.serversInfo.getOne(), "tcp", "tcp", packet, nil, &clientAddr, clientPc, start)
		}()
	}
}

func (proxy *Proxy) tcpListenerFromAddr(listenAddr *net.TCPAddr) error {
	acceptPc, err := net.ListenTCP("tcp", listenAddr)
	if err != nil {
		return err
	}
	dlog.Noticef("Now listening to %v [TCP]", listenAddr)
	go proxy.tcpListener(acceptPc)
	return nil
}

func (proxy *Proxy) exchangeWithUDPServer(serverInfo *ServerInfo, sharedKey *[32]byte, encryptedQuery []byte, clientNonce []byte) ([]byte, error) {
	pc, err := net.DialUDP("udp", nil, serverInfo.UDPAddr)
	if err != nil {
		return nil, err
	}
	pc.SetDeadline(time.Now().Add(serverInfo.Timeout))
	pc.Write(encryptedQuery)
	encryptedResponse := make([]byte, MaxDNSPacketSize)
	length, err := pc.Read(encryptedResponse)
	pc.Close()
	if err != nil {
		return nil, err
	}
	encryptedResponse = encryptedResponse[:length]
	return proxy.Decrypt(serverInfo, sharedKey, encryptedResponse, clientNonce)
}

func (proxy *Proxy) exchangeWithTCPServer(serverInfo *ServerInfo, sharedKey *[32]byte, encryptedQuery []byte, clientNonce []byte) ([]byte, error) {
	var err error
	var pc net.Conn
	proxyDialer := proxy.xTransport.proxyDialer
	if proxyDialer == nil {
		pc, err = net.Dial("tcp", serverInfo.TCPAddr.String())
	} else {
		pc, err = (*proxyDialer).Dial("tcp", serverInfo.TCPAddr.String())
	}
	if err != nil {
		return nil, err
	}
	pc.SetDeadline(time.Now().Add(serverInfo.Timeout))
	encryptedQuery, err = PrefixWithSize(encryptedQuery)
	if err != nil {
		return nil, err
	}
	pc.Write(encryptedQuery)
	encryptedResponse, err := ReadPrefixed(&pc)
	pc.Close()
	if err != nil {
		return nil, err
	}
	return proxy.Decrypt(serverInfo, sharedKey, encryptedResponse, clientNonce)
}

func (proxy *Proxy) clientsCountInc() bool {
	for {
		count := atomic.LoadUint32(&proxy.clientsCount)
		if count >= proxy.maxClients {
			return false
		}
		if atomic.CompareAndSwapUint32(&proxy.clientsCount, count, count+1) {
			dlog.Debugf("clients count: %d", count+1)
			return true
		}
	}
}

func (proxy *Proxy) clientsCountDec() {
	for {
		if count := atomic.LoadUint32(&proxy.clientsCount); count == 0 || atomic.CompareAndSwapUint32(&proxy.clientsCount, count, count-1) {
			break
		}
	}
}

func (proxy *Proxy) processIncomingQuery(serverInfo *ServerInfo, clientProto string, serverProto string, query []byte, oob []byte, clientAddr *net.Addr, clientPc net.Conn, start time.Time) {
	if len(query) < MinDNSPacketSize {
		return
	}
	pluginsState := NewPluginsState(proxy, clientProto, clientAddr, start)
	serverName := "-"
	if serverInfo != nil {
		serverName = serverInfo.Name
	}
	query, _ = pluginsState.ApplyQueryPlugins(&proxy.pluginsGlobals, query, serverName)
	if len(query) < MinDNSPacketSize || len(query) > MaxDNSPacketSize {
		return
	}
	var response []byte
	var err error
	if pluginsState.action != PluginsActionForward {
		if pluginsState.synthResponse != nil {
			response, err = pluginsState.synthResponse.PackBuffer(response)
			if err != nil {
				pluginsState.returnCode = PluginsReturnCodeParseError
				pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
				return
			}
		}
		if pluginsState.action == PluginsActionDrop {
			pluginsState.returnCode = PluginsReturnCodeDrop
			pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
			return
		}
	} else {
		pluginsState.returnCode = PluginsReturnCodeForward
	}
	if len(response) == 0 && serverInfo != nil {
		var ttl *uint32
		if serverInfo.Proto == stamps.StampProtoTypeDNSCrypt {
			sharedKey, encryptedQuery, clientNonce, err := proxy.Encrypt(serverInfo, query, serverProto)
			if err != nil {
				pluginsState.returnCode = PluginsReturnCodeParseError
				pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
				return
			}
			serverInfo.noticeBegin(proxy)
			if serverProto == "udp" {
				response, err = proxy.exchangeWithUDPServer(serverInfo, sharedKey, encryptedQuery, clientNonce)
			} else {
				response, err = proxy.exchangeWithTCPServer(serverInfo, sharedKey, encryptedQuery, clientNonce)
			}
			if err != nil {
				pluginsState.returnCode = PluginsReturnCodeServerError
				pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
				serverInfo.noticeFailure(proxy)
				return
			}
		} else if serverInfo.Proto == stamps.StampProtoTypeDoH {
			tid := TransactionID(query)
			SetTransactionID(query, 0)
			serverInfo.noticeBegin(proxy)
			resp, _, err := proxy.xTransport.DoHQuery(serverInfo.useGet, serverInfo.URL, query, proxy.timeout)
			SetTransactionID(query, tid)
			if err != nil {
				pluginsState.returnCode = PluginsReturnCodeServerError
				pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
				serverInfo.noticeFailure(proxy)
				return
			}
			response, err = ioutil.ReadAll(io.LimitReader(resp.Body, int64(MaxDNSPacketSize)))
			if err != nil {
				pluginsState.returnCode = PluginsReturnCodeServerError
				pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
				serverInfo.noticeFailure(proxy)
				return
			}
			if len(response) >= MinDNSPacketSize {
				SetTransactionID(response, tid)
			}
		} else {
			dlog.Fatal("Unsupported protocol")
		}
		if len(response) < MinDNSPacketSize || len(response) > MaxDNSPacketSize {
			pluginsState.returnCode = PluginsReturnCodeParseError
			pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
			serverInfo.noticeFailure(proxy)
			return
		}
		response, err = pluginsState.ApplyResponsePlugins(&proxy.pluginsGlobals, response, ttl)
		if err != nil {
			pluginsState.returnCode = PluginsReturnCodeParseError
			pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
			serverInfo.noticeFailure(proxy)
			return
		}
		if rcode := Rcode(response); rcode == 2 { // SERVFAIL
			dlog.Infof("Server [%v] returned temporary error code [%v] -- Upstream server may be experiencing connectivity issues", serverInfo.Name, rcode)
			serverInfo.noticeFailure(proxy)
		} else {
			serverInfo.noticeSuccess(proxy)
		}
	}
	if len(response) < MinDNSPacketSize || len(response) > MaxDNSPacketSize {
		pluginsState.returnCode = PluginsReturnCodeParseError
		pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
		if serverInfo != nil {
			serverInfo.noticeFailure(proxy)
		}
		return
	}
	if clientProto == "udp" {
		if len(response) > MaxDNSUDPPacketSize {
			response, err = TruncatedResponse(response)
			if err != nil {
				pluginsState.returnCode = PluginsReturnCodeParseError
				pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
				return
			}
		}
		raddr := interface{}(*clientAddr).(*net.UDPAddr)
		context := correctSource(oob)
		clientPc.(*net.UDPConn).WriteMsgUDP(response, context, raddr)
		if HasTCFlag(response) {
			proxy.questionSizeEstimator.blindAdjust()
		} else {
			proxy.questionSizeEstimator.adjust(ResponseOverhead + len(response))
		}
	} else {
		response, err = PrefixWithSize(response)
		if err != nil {
			pluginsState.returnCode = PluginsReturnCodeParseError
			pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
			if serverInfo != nil {
				serverInfo.noticeFailure(proxy)
			}
			return
		}
		clientPc.Write(response)
	}
	pluginsState.ApplyLoggingPlugins(&proxy.pluginsGlobals)
}

func NewProxy() Proxy {
	return Proxy{
		serversInfo: NewServersInfo(),
	}
}

func setUDPSocketOptions(conn *net.UDPConn) error {
	// Try setting the flags for both families and ignore the errors unless they
	// both error.
	err6 := ipv6.NewPacketConn(conn).SetControlMessage(ipv6.FlagDst|ipv6.FlagInterface, true)
	err4 := ipv4.NewPacketConn(conn).SetControlMessage(ipv4.FlagDst|ipv4.FlagInterface, true)
	if err6 != nil && err4 != nil {
		return err4
	}
	return nil
}

// parseDstFromOOB takes oob data and returns the destination IP.
func parseDstFromOOB(oob []byte) net.IP {
	// Start with IPv6 and then fallback to IPv4
	// TODO(fastest963): Figure out a way to prefer one or the other. Looking at
	// the lvl of the header for a 0 or 41 isn't cross-platform.
	cm6 := new(ipv6.ControlMessage)
	if cm6.Parse(oob) == nil && cm6.Dst != nil {
		return cm6.Dst
	}
	cm4 := new(ipv4.ControlMessage)
	if cm4.Parse(oob) == nil && cm4.Dst != nil {
		return cm4.Dst
	}
	return nil
}

// correctSource takes oob data and returns new oob data with the Src equal to the Dst
func correctSource(oob []byte) []byte {
	dst := parseDstFromOOB(oob)
	if dst == nil {
		return nil
	}
	// If the dst is definitely an IPv6, then use ipv6's ControlMessage to
	// respond otherwise use ipv4's because ipv6's marshal ignores ipv4
	// addresses.
	if dst.To4() == nil {
		cm := new(ipv6.ControlMessage)
		cm.Src = dst
		oob = cm.Marshal()
	} else {
		cm := new(ipv4.ControlMessage)
		cm.Src = dst
		oob = cm.Marshal()
	}
	return oob
}
