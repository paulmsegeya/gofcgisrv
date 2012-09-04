/*
Package gofcgisrv implements the webserver side of the FastCGI protocol.
*/
package gofcgisrv

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

var logger *log.Logger = log.New(os.Stderr, "", 0)

// Server is the external interface. It manages connections to a single FastCGI application.
// A server may maintain many connections, each of which may multiplex many requests.
type Server struct {
	applicationAddr string
	connections     []*conn
	reqLock         sync.Mutex
	reqCond         *sync.Cond
	initialized     bool

	// Parameters of the application
	mpx         bool
	maxConns    int
	maxRequests int

	// PHP barfs on FCGI_GET_VALUES. I don't know why. Maybe it expects a different connection.
	// For now don't do it unless asked.
	GetValues bool
}

// NewServer creates a server that will attempt to connect to the application at the given address over TCP.
func NewServer(applicationAddr string) *Server {
	s := &Server{applicationAddr: applicationAddr}
	s.maxConns = 1
	s.maxRequests = 1
	s.reqCond = sync.NewCond(&s.reqLock)
	return s
}

// Request executes a request using env and stdin as inputs and stdout and stderr as outputs.
// env should be a slice of name=value pairs. It blocks until the application has finished.
func (s *Server) Request(env []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	// Get a request. We may have to wait for one to freed up.
	r, err := s.newRequest()
	if err != nil {
		return err
	}

	// If we haven't initialized ourselves, it's time to do that.
	if s.GetValues {
		writeGetValues(r.conn.netconn, fcgiMaxConns, fcgiMaxReqs, fcgiMpxsConns, "foo")
	}

	// Send BeginRequest.
	writeBeginRequest(r.conn.netconn, r.id, fcgiResponder, 0)

	// Send the environment.
	params := newStreamWriter(r.conn.netconn, fcgiParams, r.id)
	for _, envstring := range env {
		splits := strings.SplitN(envstring, "=", 2)
		if len(splits) == 2 {
			writeNameValue(params, splits[0], splits[1])
		}
	}
	params.Close()

	r.Stdout = stdout
	r.Stderr = stderr
	// Send stdin.
	reqStdin := newStreamWriter(r.conn.netconn, fcgiStdin, r.id)
	io.Copy(reqStdin, stdin)
	reqStdin.Close()

	// Wait for end request.
	<-r.done
	return nil
}

// ServeHTTP serves an HTTP request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	env := HTTPEnv(nil, r)
	buffer := bytes.NewBuffer(nil)
	s.Request(env, r.Body, buffer, buffer)

	// Add any headers produced by the application, and skip to the response.
	ProcessResponse(buffer, w, r)
}

// Should only be called if reqLock is held.
func (s *Server) numRequests() int {
	var n = 0
	for _, c := range s.connections {
		n += c.numRequests()
	}
	return n
}

func (s *Server) newRequest() (*request, error) {
	// We may have to wait for one to become available
	s.reqLock.Lock()
	defer s.reqLock.Unlock()
	for s.numRequests() > s.maxRequests {
		s.reqCond.Wait()
	}
	// We will always need to create a new connection, for now.
	netconn, err := net.Dial("tcp", s.applicationAddr)
	if err != nil {
		return nil, err
	}
	conn := newConn(s, netconn)
	go conn.Run()
	return conn.newRequest(), nil
}

func (s *Server) releaseRequest(r *request) {
	s.reqLock.Lock()
	defer s.reqLock.Unlock()
	r.conn.removeRequest(r)
	// For now, we're telling apps to close connections, so we're done with it.
	for i, c := range s.connections {
		if c == r.conn {
			s.connections = append(s.connections[:i], s.connections[i+1:]...)
			break
		}
	}
	if r.done != nil {
		close(r.done)
	}
	s.reqCond.Signal()
}

// Conn wraps a net.Conn. It may multiplex many requests.
type conn struct {
	server   *Server
	netconn  net.Conn
	requests []*request
	numReq   int
	reqLock  sync.RWMutex
}

func newConn(s *Server, netconn net.Conn) *conn {
	return &conn{server: s, netconn: netconn}
}

func (c *conn) newRequest() *request {
	// For now, there shouldn't be anything there.
	// But pretend.
	c.reqLock.Lock()
	defer c.reqLock.Unlock()
	r := &request{conn: c}
	r.done = make(chan bool)
	c.numReq++
	for i, r := range c.requests {
		if r == nil {
			r.id = requestId(i + 1)
			c.requests[i] = r
			return r
		}
	}
	r.id = requestId(len(c.requests) + 1)
	c.requests = append(c.requests, r)
	return r
}

func (c *conn) removeRequest(r *request) {
	c.reqLock.Lock()
	defer c.reqLock.Unlock()
	idx := int(r.id) - 1
	if c.requests[idx] == r {
		c.requests[idx] = nil
		c.numReq--
	}
}

func (c *conn) numRequests() int {
	c.reqLock.Lock()
	defer c.reqLock.Unlock()
	return c.numReq
}

func (c *conn) findRequest(id requestId) *request {
	c.reqLock.Lock()
	defer c.reqLock.Unlock()
	idx := int(id) - 1
	if int(idx) >= len(c.requests) {
		return nil
	}
	return c.requests[idx]
}

func (c *conn) Run() error {
	// Sit in a loop reading records.
	for {
		rec, err := readRecord(c.netconn)
		if err != nil {
			// We're done?
			return err
		}
		// If it's a management record
		if rec.Id == 0 {
			switch rec.Type {
			case fcgiGetValuesResult:
				reader := bytes.NewReader(rec.Content)
				for {
					name, value, err := readNameValue(reader)
					if err != nil {
						break
					}
					val, err := strconv.ParseInt(value, 10, 32)
					if err != nil {
						continue
					}
					switch name {
					case fcgiMaxConns:
						c.server.maxConns = int(val)
					case fcgiMaxReqs:
						c.server.maxRequests = int(val)
					case fcgiMpxsConns:
						c.server.mpx = (val != 0)
					}
				}
			}
		} else {
			// Get the request.
			req := c.findRequest(rec.Id)
			// If there isn't one, ignore it.
			if req == nil {
				continue
			}
			switch rec.Type {
			case fcgiEndRequest:
				// We're done!
				c.server.releaseRequest(req)
			case fcgiStdout:
				// Write the data to the stdout stream
				if len(rec.Content) > 0 {
					if _, err := req.Stdout.Write(rec.Content); err != nil {
					}
				}
			case fcgiStderr:
				// Write the data to the stderr stream
				if len(rec.Content) > 0 {
					if _, err := req.Stderr.Write(rec.Content); err != nil {
					}
				}
			}
		}
	}
	return nil
}

// Request is a single request.
type request struct {
	id     requestId
	conn   *conn
	done   chan bool
	Stdout io.Writer
	Stderr io.Writer
}
