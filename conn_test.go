package enproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/getlantern/fdcount"
	"github.com/getlantern/keyman"
	"github.com/getlantern/testify/assert"
	. "github.com/getlantern/waitforserver"
)

const (
	TEXT = "Hello byte counting world"
	HR   = "----------------------------"
)

var (
	pk   *keyman.PrivateKey
	cert *keyman.Certificate

	proxyAddr     = ""
	httpAddr      = ""
	httpsAddr     = ""
	bytesReceived = int64(0)
	bytesSent     = int64(0)
	destsReceived = make(map[string]bool)
	destsSent     = make(map[string]bool)
	statMutex     sync.Mutex
)

func TestPlainTextStreaming(t *testing.T) {
	doTestPlainText(false, t)
}

func TestPlainTextBuffered(t *testing.T) {
	doTestPlainText(true, t)
}

func TestTLSStreaming(t *testing.T) {
	doTestTLS(false, t)
}

func TestTLSBuffered(t *testing.T) {
	doTestTLS(true, t)
}

func TestBadStreaming(t *testing.T) {
	doTestBad(false, t)
}

func TestBadBuffered(t *testing.T) {
	doTestBad(true, t)
}

func TestIdle(t *testing.T) {
	idleTimeout := 100 * time.Millisecond

	_, counter, err := fdcount.Matching("TCP")
	if err != nil {
		t.Fatalf("Unable to get fdcount: %v", err)
	}

	_, err = Dial(httpAddr, &Config{
		DialProxy: func(addr string) (net.Conn, error) {
			return net.Dial("tcp", proxyAddr)
		},
		IdleTimeout: idleTimeout,
	})
	if assert.NoError(t, err, "Dialing should have succeeded") {
		time.Sleep(idleTimeout * 2)
		assert.NoError(t, counter.AssertDelta(2), "All file descriptors except the connection from proxy to destination site should have been closed")
	}
}

// This test stimulates a connection leak as seen in
// https://github.com/getlantern/lantern/issues/2174.
func TestHTTPRedirect(t *testing.T) {
	startProxy(t)

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return Dial(addr, &Config{
					DialProxy: func(addr string) (net.Conn, error) {
						return net.Dial("tcp", proxyAddr)
					},
				})
			},
			DisableKeepAlives: true,
		},
	}

	_, counter, err := fdcount.Matching("TCP")
	if err != nil {
		t.Fatalf("Unable to get fdcount: %v", err)
	}

	resp, err := client.Head("http://www.facebook.com")
	if assert.NoError(t, err, "Head request to facebook should have succeeded") {
		resp.Body.Close()
	}

	assert.NoError(t, counter.AssertDelta(2), "All file descriptors except the connection from proxy to destination site should have been closed")
}

func doTestPlainText(buffered bool, t *testing.T) {
	startServers(t)

	_, counter, err := fdcount.Matching("TCP")
	if err != nil {
		t.Fatalf("Unable to get fdcount: %v", err)
	}

	conn, err := prepareConn(httpAddr, buffered, false, t)
	if err != nil {
		t.Fatalf("Unable to prepareConn: %s", err)
	}
	defer func() {
		err := conn.Close()
		assert.Nil(t, err, "Closing conn should succeed")
		assert.NoError(t, counter.AssertDelta(2), "All file descriptors except the connection from proxy to destination site should have been closed")
	}()

	doRequests(conn, t)

	assert.Equal(t, 166, bytesReceived, "Wrong number of bytes received")
	assert.Equal(t, 284, bytesSent, "Wrong number of bytes sent")
	assert.True(t, destsSent[httpAddr], "http address wasn't recorded as sent destination")
	assert.True(t, destsReceived[httpAddr], "http address wasn't recorded as received destination")
}

func doTestTLS(buffered bool, t *testing.T) {
	startServers(t)

	_, counter, err := fdcount.Matching("TCP")
	if err != nil {
		t.Fatalf("Unable to get fdcount: %v", err)
	}

	conn, err := prepareConn(httpsAddr, buffered, false, t)
	if err != nil {
		t.Fatalf("Unable to prepareConn: %s", err)
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: "localhost",
		RootCAs:    cert.PoolContainingCert(),
	})
	defer func() {
		err := conn.Close()
		assert.Nil(t, err, "Closing conn should succeed")
		assert.NoError(t, counter.AssertDelta(2), "All file descriptors except the connection from proxy to destination site should have been closed")
	}()

	err = tlsConn.Handshake()
	if err != nil {
		t.Fatalf("Unable to handshake: %s", err)
	}

	doRequests(tlsConn, t)

	assert.True(t, destsSent[httpsAddr], "https address wasn't recorded as sent destination")
	assert.True(t, destsReceived[httpsAddr], "https address wasn't recorded as received destination")
}

func doTestBad(buffered bool, t *testing.T) {
	startServers(t)

	conn, err := prepareConn(httpAddr, buffered, true, t)
	if err == nil {
		defer conn.Close()
		t.Error("Bad conn should have returned error on Connect()")
	}
}

func prepareConn(addr string, buffered bool, fail bool, t *testing.T) (conn net.Conn, err error) {
	return Dial(addr,
		&Config{
			DialProxy: func(addr string) (net.Conn, error) {
				proto := "tcp"
				if fail {
					proto = "fakebad"
				}
				return net.Dial(proto, proxyAddr)
			},
			BufferRequests: buffered,
		})
}

func doRequests(conn net.Conn, t *testing.T) {
	// Single request/response pair
	req := makeRequest(conn, t)
	readResponse(conn, req, t)

	// Consecutive request/response pairs
	req = makeRequest(conn, t)
	readResponse(conn, req, t)
}

func makeRequest(conn net.Conn, t *testing.T) *http.Request {
	req, err := http.NewRequest("GET", "http://www.google.com/humans.txt", nil)
	if err != nil {
		t.Fatalf("Unable to create request: %s", err)
	}

	go func() {
		err = req.Write(conn)
		if err != nil {
			t.Fatalf("Unable to write request: %s", err)
		}
	}()

	return req
}

func readResponse(conn net.Conn, req *http.Request, t *testing.T) {
	buffIn := bufio.NewReader(conn)
	resp, err := http.ReadResponse(buffIn, req)
	if err != nil {
		t.Fatalf("Unable to read response: %s", err)
	}

	buff := bytes.NewBuffer(nil)
	_, err = io.Copy(buff, resp.Body)
	if err != nil {
		t.Fatalf("Unable to read response body: %s", err)
	}
	text := string(buff.Bytes())
	assert.Contains(t, text, TEXT, "Wrong text returned from server")
}

func startServers(t *testing.T) {
	startHttpServer(t)
	startHttpsServer(t)
	startProxy(t)
}

func startProxy(t *testing.T) {
	if proxyAddr != "" {
		statMutex.Lock()
		bytesReceived = 0
		bytesSent = 0
		destsReceived = make(map[string]bool)
		destsReceived = make(map[string]bool)
		statMutex.Unlock()
		return
	}

	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Proxy unable to listen: %v", err)
	}
	proxyAddr = l.Addr().String()

	go func() {
		proxy := &Proxy{
			OnBytesReceived: func(clientIp string, destAddr string, req *http.Request, bytes int64) {
				statMutex.Lock()
				bytesReceived += bytes
				destsReceived[destAddr] = true
				statMutex.Unlock()
			},
			OnBytesSent: func(clientIp string, destAddr string, req *http.Request, bytes int64) {
				statMutex.Lock()
				bytesSent += bytes
				destsSent[destAddr] = true
				statMutex.Unlock()
			},
		}
		err := proxy.Serve(l)
		if err != nil {
			t.Fatalf("Proxy unable to serve: %s", err)
		}
	}()

	if err := WaitForServer("tcp", proxyAddr, 1*time.Second); err != nil {
		t.Fatal(err)
	}
}

func startHttpServer(t *testing.T) {
	if httpAddr != "" {
		return
	}

	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("HTTP unable to listen: %v", err)
	}
	httpAddr = l.Addr().String()

	doStartServer(t, l)
}

func startHttpsServer(t *testing.T) {
	if httpsAddr != "" {
		return
	}

	var err error

	pk, err = keyman.GeneratePK(2048)
	if err != nil {
		t.Fatalf("Unable to generate key: %s", err)
	}

	// Generate self-signed certificate
	cert, err = pk.TLSCertificateFor("tlsdialer", "localhost", time.Now().Add(1*time.Hour), true, nil)
	if err != nil {
		t.Fatalf("Unable to generate cert: %s", err)
	}

	keypair, err := tls.X509KeyPair(cert.PEMEncoded(), pk.PEMEncoded())
	if err != nil {
		t.Fatalf("Unable to generate x509 key pair: %s", err)
	}

	l, err := tls.Listen("tcp", "localhost:0", &tls.Config{
		Certificates: []tls.Certificate{keypair},
	})
	if err != nil {
		t.Fatalf("HTTP unable to listen: %v", err)
	}
	httpsAddr = l.Addr().String()

	doStartServer(t, l)
}

func doStartServer(t *testing.T, l net.Listener) {
	go func() {
		httpServer := &http.Server{
			Handler: http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
				resp.Write([]byte(TEXT))
			}),
		}
		err := httpServer.Serve(l)
		if err != nil {
			t.Fatalf("Unable to start http server: %s", err)
		}
	}()

	if err := WaitForServer("tcp", l.Addr().String(), 1*time.Second); err != nil {
		t.Fatal(err)
	}
}
