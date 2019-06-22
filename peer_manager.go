// Copyright 2019 cruzbit developers
// Use of this source code is governed by a MIT-style license that can be found in the LICENSE file.

package cruzbit

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/glendc/go-external-ip"
)

// PeerManager manages incoming and outgoing peer connections on behalf of the client.
// It also manages finding peers to connect to.
type PeerManager struct {
	genesisID       BlockID
	peerStore       PeerStorage
	blockStore      BlockStorage
	ledger          Ledger
	processor       *Processor
	txQueue         TransactionQueue
	dataDir         string
	myIP            string
	peer            string
	port            int
	accept          bool
	irc             bool
	inPeers         map[string]*Peer
	outPeers        map[string]*Peer
	inPeersLock     sync.RWMutex
	outPeersLock    sync.RWMutex
	addrChan        chan string
	peerNonce       string
	open            bool
	privateIPBlocks []*net.IPNet
	server          *http.Server
	shutdownChan    chan bool
	wg              sync.WaitGroup
}

// NewPeerManager returns a new PeerManager instance.
func NewPeerManager(
	genesisID BlockID, peerStore PeerStorage, blockStore BlockStorage,
	ledger Ledger, processor *Processor, txQueue TransactionQueue,
	dataDir, myExternalIP, peer string, port int, accept, irc bool) *PeerManager {

	// compute and save these
	var privateIPBlocks []*net.IPNet
	for _, cidr := range []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local addr
	} {
		_, block, _ := net.ParseCIDR(cidr)
		privateIPBlocks = append(privateIPBlocks, block)
	}

	// server to listen for and handle incoming secure WebSocket connections
	server := &http.Server{
		Addr:         "0.0.0.0:" + strconv.Itoa(port),
		TLSConfig:    tlsServerConfig, // from tls.go
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	return &PeerManager{
		genesisID:       genesisID,
		peerStore:       peerStore,
		blockStore:      blockStore,
		ledger:          ledger,
		processor:       processor,
		txQueue:         txQueue,
		dataDir:         dataDir,
		myIP:            myExternalIP, // set if upnp was enabled and successful
		peer:            peer,
		port:            port,
		accept:          accept,
		irc:             irc,
		inPeers:         make(map[string]*Peer),
		outPeers:        make(map[string]*Peer),
		addrChan:        make(chan string, 10000),
		peerNonce:       strconv.Itoa(int(rand.Int31())),
		privateIPBlocks: privateIPBlocks,
		server:          server,
		shutdownChan:    make(chan bool),
	}
}

// Run executes the PeerManager's main loop in its own goroutine.
// It determines our connectivity and manages sourcing peer addresses from seed sources
// as well as maintaining full outbound connections and accepting inbound connections.
func (p *PeerManager) Run() {
	p.wg.Add(1)
	go p.run()
}

func (p *PeerManager) run() {
	defer p.wg.Done()

	// determine external ip
	myExternalIP, err := determineExternalIP()
	if err != nil {
		log.Printf("Error determining external IP: %s\n", err)
	} else {
		log.Printf("My external IP address is: %s\n", myExternalIP)
		if len(p.myIP) != 0 {
			// if upnp enabled make sure the address returned matches the outside view
			p.open = myExternalIP == p.myIP
		} else {
			// if no upnp see if any local routable ip matches the outside view
			p.open, err = haveLocalIPMatch(myExternalIP)
			if err != nil {
				log.Printf("Error checking for local IP match: %s\n", err)
			}
		}
		p.myIP = myExternalIP
	}

	var irc *IRC
	if len(p.peer) != 0 {
		// store the explicitly specified outbound peer
		if err := p.peerStore.Store(p.peer); err != nil {
			log.Printf("Error saving peer: %s, address: %s\n", err, p.peer)
		}
	} else {
		// handle IRC seeding
		if p.irc == true {
			port := p.port
			if !p.open || !p.accept {
				// don't advertise ourself as available for inbound connections
				port = 0
			}
			irc = NewIRC()
			if err := irc.Connect(p.genesisID, port, p.addrChan); err != nil {
				log.Println(err)
			} else {
				irc.Run()
			}
		}

		// query dns seeds for peers
		addresses, err := dnsQueryForPeers()
		if err != nil {
			log.Printf("Error from DNS query: %s\n", err)
		} else {
			for _, addr := range addresses {
				log.Printf("Got peer address from DNS: %s\n", addr)
				p.addrChan <- addr
			}
		}
	}

	// handle incoming peers
	if p.accept == true {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.acceptConnections()
		}()
	}

	// try connecting to some saved peers
	p.connectToPeers()

	// try connecting out to peers every 5 minutes
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// main loop
	for {
		select {
		case addr := <-p.addrChan:
			log.Printf("Discovered peer: %s\n", addr)

			// parse, resolve and validate the address
			host, port, err := p.parsePeerAddress(addr)
			if err != nil {
				log.Printf("Peer address invalid: %s\n", err)
				continue
			}

			// store the peer
			resolvedAddr := host + ":" + port
			log.Printf("Storing peer as: %s\n", resolvedAddr)
			if err := p.peerStore.Store(resolvedAddr); err != nil {
				log.Printf("Error saving peer: %s, address: %s\n", err, resolvedAddr)
				continue
			}

			// try connecting to some saved peers
			p.connectToPeers()

		case <-ticker.C:
			// periodically try connecting to some saved peers
			p.connectToPeers()

		case _, ok := <-p.shutdownChan:
			if !ok {
				log.Println("Peer manager shutting down...")

				if irc != nil {
					// shutdown irc
					irc.Shutdown()
				}

				// shutdown http server
				p.server.Shutdown(context.Background())
				return
			}
		}
	}
}

// Shutdown stops the peer manager synchronously.
func (p *PeerManager) Shutdown() {
	close(p.shutdownChan)
	p.wg.Wait()

	// shutdown all connected peers
	var peers []*Peer
	func() {
		p.outPeersLock.RLock()
		defer p.outPeersLock.RUnlock()
		for _, peer := range p.outPeers {
			peers = append(peers, peer)
		}
	}()
	func() {
		p.inPeersLock.RLock()
		defer p.inPeersLock.RUnlock()
		for _, peer := range p.inPeers {
			peers = append(peers, peer)
		}
	}()
	for _, peer := range peers {
		peer.Shutdown()
	}

	log.Println("Peer manager shutdown")
}

func (p *PeerManager) inboundPeerCount() int {
	p.inPeersLock.RLock()
	defer p.inPeersLock.RUnlock()
	return len(p.inPeers)
}

func (p *PeerManager) outboundPeerCount() int {
	p.outPeersLock.RLock()
	defer p.outPeersLock.RUnlock()
	return len(p.outPeers)
}

// Try connecting to some recent peers
func (p *PeerManager) connectToPeers() error {
	if len(p.peer) != 0 {
		if p.outboundPeerCount() != 0 {
			// only connect to the explicitly requested peer once
			return nil
		}

		// try reconnecting to the explicit peer
		log.Printf("Attempting to connect to: %s\n", p.peer)
		if err := p.connect(p.peer); err != nil {
			log.Printf("Error connecting to peer: %s\n", p.peer)
			return err
		}
		log.Printf("Connected to peer: %s\n", p.peer)
		return nil
	}

	// otherwise try to keep us maximally connected
	want := MAX_OUTBOUND_PEER_CONNECTIONS - p.outboundPeerCount()
	if want <= 0 {
		return nil
	}
	addrs, err := p.peerStore.Get(want)
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		log.Printf("Attempting to connect to: %s\n", addr)
		if err := p.connect(addr); err != nil {
			log.Printf("Error connecting to peer: %s\n", err)
		} else {
			log.Printf("Connected to peer: %s\n", addr)
		}
	}
	return nil
}

// Connect to a peer
func (p *PeerManager) connect(addr string) error {
	peer := NewPeer(nil, p.genesisID, p.peerStore, p.blockStore, p.ledger, p.processor, p.txQueue, p.addrChan)

	if ok := p.addToOutboundSet(addr, peer); !ok {
		return fmt.Errorf("Too many peer connections")
	}

	var myAddress string
	if p.open {
		// advertise ourself as open
		myAddress = p.myIP + ":" + strconv.Itoa(p.port)
	}

	// connect to the peer
	if err := peer.Connect(addr, p.peerNonce, myAddress); err != nil {
		p.removeFromOutboundSet(addr)
		return err
	}

	peer.OnClose(func() {
		p.removeFromOutboundSet(addr)
	})
	peer.Run()

	return nil
}

// Accept incoming peer connections
func (p *PeerManager) acceptConnections() {
	// handle incoming connection upgrade requests
	peerHandler := func(w http.ResponseWriter, r *http.Request) {
		// check the peer nonce
		theirNonce := r.Header.Get("Cruzbit-Peer-Nonce")
		if theirNonce == p.peerNonce {
			log.Printf("Received connection with our own nonce")
			// write back error reply
			w.WriteHeader(http.StatusLoopDetected)
			return
		}

		// if they set their address it means they think they are open
		theirAddress := r.Header.Get("Cruzbit-Peer-Address")
		if len(theirAddress) != 0 {
			// parse, resolve and validate the address
			host, port, err := p.parsePeerAddress(theirAddress)
			if err != nil {
				log.Printf("Peer address in header is invalid: %s\n", err)

				// don't save it
				theirAddress = ""
			} else {
				// save the resolved address
				theirAddress = host + ":" + port

				// see if we're already connected outbound to them
				if p.existsInOutboundSet(theirAddress) {
					log.Printf("Already connected to %s, dropping inbound connection",
						theirAddress)
					// write back error reply
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}

				// save their address for later use
				if err := p.peerStore.Store(theirAddress); err != nil {
					log.Printf("Error saving peer: %s, address: %s\n", err, theirAddress)
				}
			}
		}

		// accept the new websocket
		conn, err := PeerUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("Upgrade:", err)
			return
		}

		peer := NewPeer(conn, p.genesisID, p.peerStore, p.blockStore, p.ledger, p.processor, p.txQueue, p.addrChan)

		if ok := p.addToInboundSet(r.RemoteAddr, peer); !ok {
			// TODO: tell the peer why
			peer.Shutdown()
			return
		}

		addr := conn.RemoteAddr().String()
		log.Printf("New peer connection from: %s", addr)
		peer.OnClose(func() {
			p.removeFromInboundSet(addr)
		})
		peer.Run()
	}

	// generate new certificate and key for tls on each run
	log.Println("Generating TLS certificate and key")
	certPath, keyPath, err := generateSelfSignedCertAndKey(p.dataDir)
	if err != nil {
		log.Println(err)
		return
	}

	// listen for websocket requests using the genesis block ID as the handler pattern
	http.HandleFunc("/"+p.genesisID.String(), peerHandler)

	log.Println("Listening for new peer connections")
	if err := p.server.ListenAndServeTLS(certPath, keyPath); err != nil {
		log.Println(err)
	}
}

// Helper to add peers to the outbound set if they'll fit
func (p *PeerManager) addToOutboundSet(addr string, peer *Peer) bool {
	p.outPeersLock.Lock()
	defer p.outPeersLock.Unlock()
	if len(p.outPeers) == MAX_OUTBOUND_PEER_CONNECTIONS {
		// too many connections
		return false
	}
	if _, ok := p.outPeers[addr]; ok {
		// already connected
		return false
	}
	p.outPeers[addr] = peer
	log.Printf("Outbound peer count: %d\n", len(p.outPeers))
	return true
}

// Helper to add peers to the inbound set if they'll fit
func (p *PeerManager) addToInboundSet(addr string, peer *Peer) bool {
	p.inPeersLock.Lock()
	defer p.inPeersLock.Unlock()
	if len(p.inPeers) == MAX_INBOUND_PEER_CONNECTIONS {
		// too many connections
		return false
	}
	if _, ok := p.inPeers[addr]; ok {
		// already connected
		return false
	}
	p.inPeers[addr] = peer
	log.Printf("Inbound peer count: %d\n", len(p.inPeers))
	return true
}

// Helper to check if a peer address exists in the outbound set
func (p *PeerManager) existsInOutboundSet(addr string) bool {
	p.outPeersLock.RLock()
	defer p.outPeersLock.RUnlock()
	_, ok := p.outPeers[addr]
	return ok
}

// Helper to remove peers from the outbound set
func (p *PeerManager) removeFromOutboundSet(addr string) {
	p.outPeersLock.Lock()
	defer p.outPeersLock.Unlock()
	delete(p.outPeers, addr)
	log.Printf("Outbound peer count: %d\n", len(p.outPeers))
}

// Helper to remove peers from the inbound set
func (p *PeerManager) removeFromInboundSet(addr string) {
	p.inPeersLock.Lock()
	defer p.inPeersLock.Unlock()
	delete(p.inPeers, addr)
	log.Printf("Inbound peer count: %d\n", len(p.inPeers))
}

// Parse, resolve and validate peer addreses
func (p *PeerManager) parsePeerAddress(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("Malformed peer address: %s", addr)
	}

	// sanity check port
	portInt, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", "", fmt.Errorf("Invalid port in peer address: %s", addr)
	}

	// resolve the host to an ip
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", "", fmt.Errorf("Unable to resolve IP address for: %s, error: %s", host, err)
	}
	if len(ips) == 0 {
		return "", "", fmt.Errorf("No IP address found for peer address: %s", addr)
	}

	// don't accept ourself
	if p.myIP == ips[0].String() && p.port == int(portInt) {
		return "", "", fmt.Errorf("Peer address is ours: %s", addr)
	}

	// filter out local networks
	for _, block := range p.privateIPBlocks {
		if block.Contains(ips[0]) {
			return "", "", fmt.Errorf("IP is in local address space: %s", ips[0])
		}
	}

	return ips[0].String(), port, nil
}

// Do any of our local IPs match our external IP?
func haveLocalIPMatch(externalIP string) (bool, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false, err
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.String() == externalIP {
				return true, nil
			}
		}
	}
	return false, nil
}

// Determine external IP address
func determineExternalIP() (string, error) {
	consensus := externalip.DefaultConsensus(nil, nil)
	ip, err := consensus.ExternalIP()
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}