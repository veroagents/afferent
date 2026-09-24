package brain

import "net/http"

// NoRedirects returns a copy of hc (a plain client when nil) with
// redirects refused, unless hc already sets its own policy. Go's default
// policy re-sends Authorization on a same-host redirect even when it
// downgrades https to http, and a 307/308 re-sends the whole body (runtime
// events, MCP tool calls), so every client that carries an afferent bearer
// token to brainsrv stops at a 3xx and reports it as an error instead.
func NoRedirects(hc *http.Client) *http.Client {
	if hc == nil {
		hc = &http.Client{}
	}
	if hc.CheckRedirect != nil {
		return hc
	}
	c := *hc
	c.CheckRedirect = RefuseRedirect
	return &c
}

// RefuseRedirect is an http.Client CheckRedirect that stops at any 3xx.
func RefuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
