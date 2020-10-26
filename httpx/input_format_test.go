package httpx

import (
	"bufio"
	"errors"
	"io/ioutil"
	"net/http"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/gramLabs/vhs/flow"
	"github.com/gramLabs/vhs/middleware"
	"github.com/gramLabs/vhs/session"
)

type testMiddleware struct {
	expectedErr error
}

func (m *testMiddleware) Start() error { return nil }
func (m *testMiddleware) Wait() error  { return nil }
func (m *testMiddleware) Close()       {}

func (m *testMiddleware) Exec(_ session.Context, header []byte, n interface{}) (interface{}, error) {
	if m.expectedErr != nil {
		return nil, m.expectedErr
	}
	switch r := n.(type) {
	case *Request:
		r.RequestURI += " 111"
	case *Response:
		r.Status += " 111"
	}
	return n, nil
}

func newTestReadCloserID(s string) flow.InputReader {
	var (
		sr = strings.NewReader(s)
		br = bufio.NewReader(sr)
	)
	return ioutil.NopCloser(br)
}

func TestInputFormatInit(t *testing.T) {
	cases := []struct {
		desc        string
		m           middleware.Middleware
		r           flow.InputReader
		msgs        []Message
		count       int
		sessionID   string
		errContains string
	}{
		{
			desc: "empty",
			r:    ioutil.NopCloser(strings.NewReader("")),
		},
		{
			desc:  "no middleware",
			r:     newTestReadCloserID("GET /111.html HTTP/1.1\r\nheader:foo\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n"),
			count: 2,
			msgs: []Message{
				&Request{
					Method:     "GET",
					URL:        newURL("/111.html"),
					Proto:      "HTTP/1.1",
					ProtoMajor: 1,
					ProtoMinor: 1,
					Header:     http.Header{"Header": {"foo"}},
					MimeType:   "text/plain; charset=utf-8",
					Cookies:    []*http.Cookie{},
					RequestURI: "/111.html",
				},
				&Response{
					Status:     "204 No Content",
					StatusCode: 204,
					Proto:      "HTTP/1.1",
					ProtoMajor: 1,
					ProtoMinor: 1,
					Header:     http.Header{},
					Cookies:    []*http.Cookie{},
				},
			},
		},
		{
			desc:  "middleware",
			r:     newTestReadCloserID("GET /111.html HTTP/1.1\r\nheader:foo\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n"),
			count: 2,
			m:     &testMiddleware{},
			msgs: []Message{
				&Request{
					Method:     "GET",
					URL:        newURL("/111.html"),
					Proto:      "HTTP/1.1",
					ProtoMajor: 1,
					ProtoMinor: 1,
					Header:     http.Header{"Header": {"foo"}},
					MimeType:   "text/plain; charset=utf-8",
					Cookies:    []*http.Cookie{},
					RequestURI: "/111.html 111",
				},
				&Response{
					Status:     "204 No Content 111",
					StatusCode: 204,
					Proto:      "HTTP/1.1",
					ProtoMajor: 1,
					ProtoMinor: 1,
					Cookies:    []*http.Cookie{},
					Header:     http.Header{},
				},
			},
		},
		{
			desc:  "middleware error",
			r:     newTestReadCloserID("GET /111.html HTTP/1.1\r\nheader:foo\r\n\r\n"),
			count: 0,
			m: &testMiddleware{
				expectedErr: errors.New("111"),
			},
			errContains: "111",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			errs := make(chan error, 10)
			ctx, _, _ := session.NewContexts(&session.Config{Debug: true}, errs)
			ctx.SessionID = c.sessionID
			inputFormat, err := NewInputFormat(ctx)
			assert.NilError(t, err)

			go inputFormat.Init(ctx, c.m, c.r)

			var (
				msgs []Message
				out  = inputFormat.Out()
			)
			for i := 0; i < c.count; i++ {
				m := makeComparable((<-out).(Message))
				msgs = append(msgs, m)
			}

			time.Sleep(50 * time.Millisecond)
			ctx.Cancel()

			if c.errContains == "" {
				assert.DeepEqual(t, msgs, c.msgs)
			} else {
				assert.ErrorContains(t, <-errs, c.errContains)
			}
		})
	}
}
