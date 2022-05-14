package restserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Conn defines the methods required for a redis connection. It is a subset
// of the popular redigo Conn interface.
type Conn interface {
	// Close closes the connection.
	Close() error

	// Do sends a command to the server and returns the received reply.
	// This function will use the timeout which was set when the connection is created
	Do(commandName string, args ...interface{}) (reply interface{}, err error)
}

type Server struct {
	APIToken    string
	GetConnFunc func(context.Context) Conn

	mu         sync.Mutex // protects the rest token map
	restTokens map[string]auth
}

type auth struct {
	Username string
	Password string
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userPass, ok := s.authenticate(requestToken(r))
	if !ok {
		reply(w, errorResult{"Unauthorized"}, http.StatusUnauthorized)
		return
	}

	// only GET or POST methods are allowed
	if r.Method != "GET" && r.Method != "POST" {
		reply(w, nil, http.StatusMethodNotAllowed) // no body returned in that case
		return
	}

	// read the full body, we need to know if there is one, and if so we need it
	// all.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		reply(w, errorResult{err.Error()}, http.StatusInternalServerError)
		return
	}

	conn := s.GetConnFunc(r.Context())
	defer conn.Close()

	// might need to authenticate the connection with the proper user-password
  if userPass != auth{} {
  }

	// both GET and POST are supported regardless of how data is sent (path,
	// body, query string). We switch on the path with any trailing slash
	// removed.
	switch path := r.URL.Path; strings.TrimSuffix(path, "/") {
	case "":
		var args []interface{}

		// a full single command in the body (a single array)
		if err := json.Unmarshal(body, &args); err != nil {
			reply(w, errorResult{"ERR failed to parse command"}, http.StatusBadRequest)
			return
		}
		if len(args) == 0 {
			reply(w, errorResult{"ERR empty command"}, http.StatusBadRequest)
			return
		}

		cmd := fmt.Sprint(args[0])
		v, code := s.execCmd(conn, cmd, args[1:]...)
		reply(w, v, code)
		return

	case "/pipeline":
		var cmds [][]interface{}

		// multiple full commands in the body (an array of arrays)
		if err := json.Unmarshal(body, &cmds); err != nil {
			reply(w, errorResult{"ERR failed to parse pipeline request"}, http.StatusBadRequest)
			return
		}
		if len(cmds) == 0 {
			reply(w, errorResult{"ERR empty pipeline request"}, http.StatusBadRequest)
			return
		}

		// execute pipeline one at a time, no atomic guarantee in upstash pipeline
		var results []interface{}
		for _, cmd := range cmds {
			if len(cmd) == 0 {
				results = append(results, errorResult{"ERR empty pipeline command"})
				continue
			}
			cmdName := fmt.Sprint(cmd[0])
			v, _ := s.execCmd(conn, cmdName, cmd[1:]...)
			results = append(results, v)
		}
		reply(w, results, http.StatusOK)
		return

	default:
		// the single command is made of the path, optional body and optional query
		segments := strings.Split(path, "/")
		// remove the first segment which will always be empty
		segments = segments[1:]

		// if there's a body, it comes after the path segments
		if len(body) > 0 {
			segments = append(segments, string(body))
		}

		// if there are query values, they come last
		qparts := strings.Split(r.URL.RawQuery, "&")
		for _, qpart := range qparts {
			// if the query key has a value, then it becomes 2 redis arguments, e.g.
			// EX=100.
			kv := strings.SplitN(qpart, "=", 2)
			segments = append(segments, kv...)
		}

		args := make([]interface{}, len(segments)-1)
		for i, v := range segments[1:] {
			args[i] = v
		}
		v, code := s.execCmd(conn, segments[0], args...)
		reply(w, v, code)
		return
	}
}

type errorResult struct {
	Error string `json:"error"`
}

type successResult struct {
	Result interface{} `json:"result"`
}

func (s *Server) execCmd(conn Conn, cmd string, args ...interface{}) (interface{}, int) {
	if strings.ToLower(cmd) == "acl" && len(args) > 0 && strings.ToLower(fmt.Sprint(args[0])) == "resttoken" {
		return s.execACLRestToken(conn, cmd, args...)
	}

	res, err := conn.Do(cmd, args...)
	if err != nil {
		return errorResult{Error: err.Error()}, http.StatusBadRequest
	}
	return successResult{Result: res}, http.StatusOK
}

func (s *Server) execACLRestToken(conn Conn, cmd string, args ...interface{}) (interface{}, int) {
	if len(args) != 3 { // RESTTOKEN <username> <password>
		return errorResult{Error: "ERR invalid syntax. Usage: ACL RESTTOKEN username password"}, http.StatusBadRequest
	}

	user, pwd := fmt.Sprint(args[1]), fmt.Sprint(args[2])
	// attempt a connection with username and password, and if successful,
	// generate a token associated with it.
	vAuth, code := s.execCmd(conn, "AUTH", user, pwd)
	if code != http.StatusOK {
		return vAuth, code
	}

	// auth succeeded, generate the associated token
	res, err := conn.Do("ACL", "GENPASS")
	if err != nil {
		return errorResult{Error: err.Error()}, http.StatusBadRequest
	}
	token := res.(string)

	s.mu.Lock()
	if s.restTokens == nil {
		s.restTokens = make(map[string]auth)
	}
	s.restTokens[token] = auth{user, pwd}
	s.mu.Unlock()

	return successResult{Result: res}, http.StatusOK
}

func reply(w http.ResponseWriter, v interface{}, status int) {
	w.Header().Add("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func requestToken(r *http.Request) string {
	// token is either in Authorization header or _token query string
	tok := r.URL.Query().Get("_token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return tok
}

func (s *Server) authenticate(tok string) (auth, bool) {
	if tok == s.APIToken {
		return auth{}, true
	}

	// else look for ACL RESTTOKEN authentication...
	s.mu.Lock()
	userPass, ok := s.restTokens[tok]
	s.mu.Unlock()

	if !ok {
		return auth{}, false
	}
	return userPass, true
}
