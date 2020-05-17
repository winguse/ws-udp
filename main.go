package main

import (
	"crypto/rand"
	"flag"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"net/http"

	"golang.org/x/net/websocket"
)

// SessionKeyLength the session key length
const SessionKeyLength = 16

var timeout = flag.Int("timeout", 30, "session timeout in seconds")
var restartSleep = flag.Int("restart-sleep", 3, "restart sleep in seconds")
var verboseLoging = flag.Bool("verbose", false, "verbose logging")
var bufferSize = flag.Int("buffer-size", 1600, "buffer size in bytes, the max UDP package size.")

var clientMode = flag.Bool("client-mode", false, "running mode, true for client mode, false for server mode")

var clientFrom = flag.String("client-from", "127.0.0.1:2000", "client udp listening address")
var clientTo = flag.String("client-to", "ws://127.0.0.1:3000/path", "client's destination web socket address")

var serverAddress = flag.String("server-address", "127.0.0.1:3000", "server listen address")
var serverPath = flag.String("server-path", "/path", "server websocket path")
var serverTo = flag.String("server-to", "127.0.0.1:4000", "server will sent the udp package to")

var buffPool sync.Pool

func getBuf() []byte {
	return buffPool.Get().([]byte)
}

func returnBuf(buf []byte) {
	c := cap(buf)
	buf = buf[:c]
	buffPool.Put(buf)
}

func verbosePrintf(format string, v ...interface{}) {
	if *verboseLoging {
		log.Printf(format, v...)
	}
}

type serverSession struct {
	done   bool
	toConn *net.UDPConn
	wg     *sync.WaitGroup
	ch     chan []byte
}

func (s *serverSession) Done() {
	s.wg.Done()
	if !s.done {
		s.done = true
		close(s.ch)
		s.toConn.Close()
	}
}

func server() {
	toAddr, err := net.ResolveUDPAddr("udp", *serverTo)
	if err != nil {
		log.Fatal(err)
	}

	sessions := make(map[[SessionKeyLength]byte]*serverSession)

	http.Handle(*serverPath, websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		log.Printf("got ws connection")
		buf := make([]byte, *bufferSize)
		n, err := ws.Read(buf)
		if n != SessionKeyLength || err != nil {
			log.Printf("expected session ID, but it's not.")
			return
		}
		sessionID := [SessionKeyLength]byte{}
		for i := 0; i < SessionKeyLength; i++ {
			sessionID[i] = buf[i]
		}
		session, ok := sessions[sessionID]

		if !ok {
			// new session
			log.Printf("%x new session\n", sessionID)
			toConn, err := net.DialUDP("udp", nil, toAddr)
			if err != nil {
				log.Printf("error while dial to server target, close ws\n")
				return
			}
			session = &serverSession{
				done:   false,
				toConn: toConn,
				wg:     &sync.WaitGroup{},
				ch:     make(chan []byte),
			}
			sessions[sessionID] = session
			// copy from target -> channel
			session.wg.Add(1)
			go func() {
				defer session.Done()
				for !session.done {
					buf := getBuf()
					toConn.SetReadDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
					n, _, err := toConn.ReadFromUDP(buf)
					if err != nil {
						log.Printf("Error while read from target, %s\n", err)
						returnBuf(buf)
						break
					}
					session.ch <- buf[:n]
				}
			}()
			// remove session
			go func() {
				session.wg.Wait()
				delete(sessions, sessionID)
			}()
		} else {
			log.Printf("%x session got new connection\n", sessionID)
		}

		// server -> target
		session.wg.Add(1)
		go func() {
			defer session.Done()
			data := make([]byte, *bufferSize)
			for !session.done {
				ws.SetReadDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
				n, err := ws.Read(data)
				if err != nil {
					log.Printf("Error while read from ws, %s\n", err)
					break
				}
				verbosePrintf("server -> target %s, size: %d\n", toAddr, n)
				_, err = session.toConn.Write(data[:n])
				if err != nil {
					log.Printf("Error while write to target, %s\n", err)
					break
				}
			}
		}()

		// channel -> client
		session.wg.Add(1)
		go func() {
			defer session.Done()
			for !session.done {
				buf, ok := <-session.ch
				if !ok || session.done {
					break
				}
				_, err = ws.Write(buf)
				returnBuf(buf)
				if err != nil {
					log.Printf("Error while write to ws, %s\n", err)
					break
				}
			}
		}()

		log.Printf("%x ws session running\n", sessionID)
		session.wg.Wait()
		log.Printf("%x ws session done\n", sessionID)
	}))
	log.Printf("Server working on %s (%s) -> %s\n", *serverAddress, *serverPath, *serverTo)
	err = http.ListenAndServe(*serverAddress, nil)
	if err != nil {
		log.Printf("ListenAndServe %s ", err.Error())
	}
}

func client() {
	sessionID := make([]byte, SessionKeyLength)
	rand.Read(sessionID)

	fromAddr, err := net.ResolveUDPAddr("udp", *clientFrom)
	if err != nil {
		log.Fatal(err)
	}
	localConn, err := net.ListenUDP("udp", fromAddr)
	if err != nil {
		log.Printf("error while client listening %s", err)
	}
	defer localConn.Close()

	var clientAddr *net.UDPAddr // assuming we only have one client, creating one to one mapping

	remotes := strings.Split(*clientTo, ",")

	var wg sync.WaitGroup
	var done = false
	ch := make(chan []byte)
	setDone := func() {
		if !done {
			done = true
			close(ch)
		}
		wg.Done()
	}

	// receive local connection and put into channel
	wg.Add(1)
	go func() {
		defer setDone()
		for !done {
			buf := getBuf()
			var n int
			var err error
			n, clientAddr, err = localConn.ReadFromUDP(buf)
			if err != nil {
				log.Printf("error during read: %s\n", err)
				returnBuf(buf)
				break
			}
			if done {
				break
			}
			verbosePrintf("client <%s> -> server, size: %d\n", clientAddr, n)
			ch <- buf[:n]
		}
	}()

	var created = 0
	for _, remote := range remotes {
		origin := "http://localhost/"
		ws, err := websocket.Dial(remote, "", origin)
		if err != nil {
			log.Printf("error while client dial ws to %s: %s", remote, err)
			continue // skip one conn
		}
		created = created + 1
		defer ws.Close()
		ws.Write(sessionID)

		// client -> server
		wg.Add(1)
		go func() {
			defer setDone()
			for !done {
				buf, ok := <-ch
				if !ok || done {
					break
				}
				_, err := ws.Write(buf)
				returnBuf(buf)
				if err != nil {
					log.Printf("error send data to ws: %s\n", err)
					break
				}
			}
		}()

		// server -> client
		wg.Add(1)
		go func() {
			defer setDone()
			data := make([]byte, *bufferSize)
			for !done {
				var n int
				ws.SetReadDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
				n, err = ws.Read(data)
				if err != nil {
					log.Printf("error during ws read: %s\n", err)
					break
				}
				if clientAddr == nil {
					log.Println("client addr nil, skip sending...")
					continue
				}
				verbosePrintf("server -> client <%s>, size: %d\n", clientAddr, n)
				localConn.WriteToUDP(data[:n], clientAddr)
			}
		}()
	}

	if created == 0 {
		done = true
		close(ch)
	}

	log.Printf("Client working on %s -> %s\n", *clientFrom, *clientTo)
	wg.Wait()
	log.Println("Client terminated")
}

func main() {
	flag.Parse()

	// BuffPool buffer pool
	buffPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, *bufferSize)
		},
	}

	for {
		if *clientMode {
			client()
		} else {
			server()
		}
		log.Printf("restarting in %d seconds...\n", *restartSleep)
		time.Sleep(time.Duration(*restartSleep) * time.Second)
	}
}
