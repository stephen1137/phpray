// Package main implements a Model Context Protocol server for PHPRay.
//
// The protocol is JSON-RPC 2.0 over stdio. We speak it directly rather than
// pulling in an SDK: PHPRay ships as binaries with no dependencies and the
// three methods an agent needs (initialize, tools/list, tools/call) are small.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const protocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// toolSpec is one tool as advertised to the agent.
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// call runs the tool; it returns text for the agent, or an error.
	call func(args map[string]any) (string, error) `json:"-"`
	// zapis oznacza narzędzie, które zmienia stan po stronie serwera.
	// Publiczna końcówka HTTP bez tokenu takich nie pokazuje: konto demo ma
	// rolę tylko do odczytu, więc konsola i tak odmówi (403), a narzędzie
	// na liście, którego nie da się użyć, to obietnica bez pokrycia.
	zapis bool `json:"-"`
}

type server struct {
	out   *bufio.Writer
	mu    sync.Mutex
	tools []toolSpec
}

func (s *server) send(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *server) reply(id json.RawMessage, result any) {
	s.send(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) fail(id json.RawMessage, code int, msg string) {
	s.send(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// run reads requests until stdin closes.
func (s *server) run(in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue // a malformed line is not worth tearing the session down
		}
		s.handle(req)
	}
}

func (s *server) handle(req rpcRequest) {
	if odp := s.odpowiedz(req); odp != nil {
		s.send(*odp)
	}
}

// odpowiedz liczy odpowiedź na jedno żądanie i nic nie wypisuje. Dzięki temu
// ten sam dispatch obsługuje stdio i HTTP: transport decyduje, dokąd trafia
// wynik, a powiadomienie (żądanie bez id) zwraca nil.
func (s *server) odpowiedz(req rpcRequest) *rpcResponse {
	ok := func(result any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	zle := func(code int, msg string) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}
	switch req.Method {
	case "initialize":
		return ok(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "phpray", "version": version},
		})
	case "notifications/initialized", "notifications/cancelled":
		// notifications carry no id and expect no answer
		return nil
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		return ok(map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return zle(-32602, "invalid params")
		}
		for _, t := range s.tools {
			if t.Name != p.Name {
				continue
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			text, err := t.call(p.Arguments)
			if err != nil {
				return ok(map[string]any{
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
					"isError": true,
				})
			}
			return ok(map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			})
		}
		return zle(-32601, fmt.Sprintf("unknown tool %q", p.Name))
	default:
		if len(req.ID) > 0 {
			return zle(-32601, "method not found: "+req.Method)
		}
	}
	return nil
}
