package main

import (
	"flag"
	"log"
	"net"
	"sync"
	"time"

	"net/http"

	"golang.org/x/net/websocket"
)

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

func verbosePrintf(format string, v ...interface{}) {
	if *verboseLoging {
		log.Printf(format, v...)
	}
}

func server() {
	toAddr, err := net.ResolveUDPAddr("udp", *serverTo)
	if err != nil {
		log.Fatal(err)
	}

	http.Handle(*serverPath, websocket.Handler(func(ws *websocket.Conn) {
		log.Printf("got ws connection")
		toConn, err := net.DialUDP("udp", nil, toAddr)
		if err != nil {
			log.Printf("error while dial to server target, close ws\n")
			ws.Close()
			return
		}
		var wg sync.WaitGroup
		wg.Add(2)
		var done = false
		setDone := func() {
			defer wg.Done()
			done = true
		}
		// server -> target
		go func() {
			defer setDone()
			data := make([]byte, *bufferSize)
			for {
				if done {
					break
				}
				ws.SetReadDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
				n, err := ws.Read(data)
				if err != nil {
					log.Printf("Error while read from ws, %s\n", err)
					break
				}
				if done {
					break
				}
				verbosePrintf("server -> target %s, size: %d\n", toAddr, n)
				_, err = toConn.Write(data[:n])
				if err != nil {
					log.Printf("Error while write to target, %s\n", err)
					break
				}
			}
		}()
		// target -> server
		go func() {
			defer setDone()
			data := make([]byte, *bufferSize)
			for {
				if done {
					break
				}
				toConn.SetReadDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
				n, _, err := toConn.ReadFromUDP(data)
				if err != nil {
					log.Printf("Error while read from target, %s\n", err)
					break
				}
				if done {
					break
				}
				verbosePrintf("target %s -> server, size: %d\n", toAddr, n)
				_, err = ws.Write(data[:n])
				if err != nil {
					log.Printf("Error while write to ws, %s\n", err)
					break
				}
			}
		}()
		wg.Wait()
		ws.Close()
		log.Println("ws connection closed")
	}))
	log.Printf("Server working on %s (%s) -> %s\n", *serverAddress, *serverPath, *serverTo)
	err = http.ListenAndServe(*serverAddress, nil)
	if err != nil {
		log.Printf("ListenAndServe %s ", err.Error())
	}
}

func client() {
	fromAddr, err := net.ResolveUDPAddr("udp", *clientFrom)
	if err != nil {
		log.Fatal(err)
	}
	localConn, err := net.ListenUDP("udp", fromAddr)
	if err != nil {
		log.Printf("error while client listening %s", err)
	}
	defer localConn.Close()

	origin := "http://localhost/"
	ws, err := websocket.Dial(*clientTo, "", origin)
	if err != nil {
		log.Printf("error while client dial ws %s", err)
		return
	}
	defer ws.Close()

	var clientAddr *net.UDPAddr // assuming we only have one client, creating one to one mapping

	var wg sync.WaitGroup
	wg.Add(2)
	var done = false
	setDone := func() {
		defer wg.Done()
		done = true
	}

	// client -> server
	go func() {
		defer setDone()
		data := make([]byte, *bufferSize)
		for {
			if done {
				break
			}
			var n int
			ws.SetReadDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
			n, clientAddr, err = localConn.ReadFromUDP(data)
			if err != nil {
				log.Printf("error during read: %s\n", err)
				break
			}
			if done {
				break
			}
			verbosePrintf("client <%s> -> server, size: %d\n", clientAddr, n)
			if _, err := ws.Write(data[:n]); err != nil {
				log.Printf("error send data to ws: %s\n", err)
				break
			}
		}
	}()

	// server -> client
	go func() {
		defer setDone()
		data := make([]byte, *bufferSize)
		for {
			if done {
				break
			}
			var n int
			ws.SetReadDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
			n, err = ws.Read(data)
			if err != nil {
				log.Printf("error during ws read: %s\n", err)
				break
			}
			if done {
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

	log.Printf("Client working on %s -> %s\n", *clientFrom, *clientTo)
	wg.Wait()
}

func main() {
	flag.Parse()
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
