package mcpproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Messages on an SSE answer reach the client as they arrive. brainsrv may
// send a request (sampling, elicitation, roots) on the stream and wait for
// the client's reply before it answers: buffering until the answer would
// hang the tool call forever.
func TestProxyStreamsSSEBeforeTheAnswer(t *testing.T) {
	replied := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		switch {
		case m.Method == "initialize":
			w.Header().Set(HeaderSession, "sid-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, m.ID)
		case m.Method == "" && string(m.ID) == `"s1"`: // the client's reply
			close(replied)
			w.WriteHeader(http.StatusAccepted)
		case m.Method == "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"s1\",\"method\":\"sampling/createMessage\",\"params\":{}}\n\n")
			fl.Flush()
			select {
			case <-replied:
			case <-time.After(5 * time.Second):
				return // no answer: the proxy reports an error
			}
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"ok\":true}}\n\n", m.ID)
			fl.Flush()
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	s := start(t, Options{URL: srv.URL, Context: "afferent-poc", Scope: "s", Tokens: &fakeTokens{cur: "t1"}})
	s.send(initMsg)
	s.wait(1)
	s.send(initializedMsg)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall"}}`)
	lines := s.wait(3) // progress and the server's request, before any answer
	if p := parse(t, lines[1]); p.Method != "notifications/progress" {
		t.Fatalf("progress not relayed first: %q", lines)
	}
	if p := parse(t, lines[2]); p.Method != "sampling/createMessage" || string(p.ID) != `"s1"` {
		t.Fatalf("server request not relayed: %q", lines)
	}
	s.send(`{"jsonrpc":"2.0","id":"s1","result":{"content":"x"}}`)
	lines = s.wait(4)
	if r := byID(t, lines, "2"); r.Error != nil || r.Result["ok"] != true {
		t.Fatalf("answer %+v (lines %q)", r, lines)
	}
	if !strings.Contains(lines[3], `"ok":true`) {
		t.Fatalf("order %q", lines)
	}
	s.close()
}
