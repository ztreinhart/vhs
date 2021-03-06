package httpx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rename-this/vhs/core"
)

// HAR is an HTTP Archive.
// https://w3c.github.io/web-performance/specs/HAR/Overview.html
// http://www.softwareishard.com/blog/har-12-spec/
type HAR struct {
	in       chan interface{}
	complete chan struct{}
}

// NewHAR creates a mew HAR format.
func NewHAR(ctx core.Context) (core.OutputFormat, error) {
	registerEnvelopes(ctx)
	return &HAR{
		in:       make(chan interface{}),
		complete: make(chan struct{}, 1),
	}, nil
}

// In returns the input channel.
func (h *HAR) In() chan<- interface{} { return h.in }

// Complete returns a completion channel.
func (h *HAR) Complete() <-chan struct{} {
	return h.complete
}

// Init initializes the HAR sink.
func (h *HAR) Init(ctx core.Context, w io.Writer) {
	ctx.Logger = ctx.Logger.With().
		Str(core.LoggerKeyComponent, "har").
		Logger()

	ctx.Logger.Debug().Msg("init")

	defer func() {
		h.complete <- struct{}{}
	}()

	c := NewCorrelator(ctx.FlowConfig.HTTPTimeout)
	c.Start(ctx)

	ctx.Logger.Debug().Msg("correlator started")

	hh := &har{
		Log: harLog{
			Version: "1.2",
			Creator: harCreator{
				Name:    "vhs",
				Version: "0.0.1",
			},
		},
	}

	go func() {
		for {
			select {
			case n := <-h.in:
				switch m := n.(type) {
				case Message:
					c.Messages <- m
					if ctx.Config.DebugHTTPMessages {
						ctx.Logger.Debug().Interface("m", m).Msg("received message")
					} else {
						ctx.Logger.Debug().Msg("received message")
					}
				}
			case <-ctx.StdContext.Done():
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case r := <-c.Exchanges:
				h.addRequest(ctx, hh, r)
				if ctx.Config.DebugHTTPMessages {
					ctx.Logger.Debug().Interface("r", r).Msg("received request from correlator")
				} else {
					ctx.Logger.Debug().Msg("received request from correlator")
				}
			case <-ctx.StdContext.Done():
				return
			}
		}
	}()

	<-ctx.StdContext.Done()

	ctx.Logger.Debug().Msg("context canceled")

	if err := json.NewEncoder(w).Encode(hh); err != nil {
		ctx.Errors <- fmt.Errorf("failed to encode to JSON: %w", err)
	}
}

func (h *HAR) addRequest(ctx core.Context, hh *har, req *Request) {
	ensureAbsoluteURL(req)

	request := harRequest{
		Method:      req.Method,
		URL:         req.URL.String(),
		HTTPVersion: req.Proto,
		Cookies:     extractCookies(req.Cookies),
		Headers:     mapToHarNVP(req.Header),
		QueryString: mapToHarNVP(req.URL.Query()),
		PostData:    extractPostData(req),
		HeadersSize: -1,
		BodySize:    len(req.Body),
	}

	var (
		response  harResponse
		roundtrip int64
	)

	if req.Response != nil {
		content := harContent{
			Size:     req.Response.ContentLength,
			MimeType: req.Response.Header.Get("Content-Type"),
			Text:     req.Response.Body,
		}

		response = harResponse{
			Status:      req.Response.StatusCode,
			StatusText:  req.Response.Status,
			HTTPVersion: req.Response.Proto,
			Cookies:     extractCookies(req.Response.Cookies),
			Headers:     mapToHarNVP(req.Response.Header),
			Content:     content,
			RedirectURL: req.Response.Location,
			HeadersSize: -1,
			BodySize:    len(req.Response.Body),
		}

		roundtrip = req.Response.Created.Sub(req.Created).Milliseconds()
	}

	entry := harEntry{
		StartedDateTime: req.Created.Format(time.RFC3339),
		Time:            roundtrip,
		Request:         request,
		Response:        response,
		Cache:           harCache{},
		Timings: harEntryTiming{
			Send:    1,
			Wait:    1,
			Receive: 1,
		},
		ServerIPAddress: req.ServerAddr,
		Connection:      req.GetConnectionID(),
	}

	if ctx.Config.DebugHTTPMessages {
		ctx.Logger.Debug().Interface("entry", entry).Msg("adding entry")
	} else {
		ctx.Logger.Debug().Msg("adding entry")
	}

	hh.Log.Entries = append(hh.Log.Entries, entry)
}

// HELPERS, ETC.

// extractCookies pulls the cookies out of a cookie slice ([]*http.Cookie) as generated when parsing an http request
// or response.
func extractCookies(cookies []*http.Cookie) []harCookie {
	if cookies == nil || len(cookies) == 0 {
		return []harCookie{}
	}

	harCookies := make([]harCookie, len(cookies))
	for i, cookie := range cookies {
		if cookie != nil {
			harCookies[i] = harCookie{
				Name:     cookie.Name,
				Value:    cookie.Value,
				Path:     cookie.Path,
				Domain:   cookie.Domain,
				HTTPOnly: cookie.HttpOnly,
				Secure:   cookie.Secure,
				Comment:  "",
			}

			if cookie.Expires.IsZero() {
				harCookies[i].Expires = ""
			} else {
				harCookies[i].Expires = cookie.Expires.Format(time.RFC3339)
			}
		}
	}
	return harCookies
}

// extractPostData pulls the POST data out of a Request into a harPOST.
// Data of type application/x-www-form-urlencoded is parsed into name-value pairs and stored in the Params field.
// Data of other types (including multipart/form-data) is not parsed and is included as-is in the Text field.
// Field Params and field Text are mutually exclusive.
func extractPostData(req *Request) harPOST {
	var post harPOST
	if req.Method != http.MethodPost {
		return post
	}

	post.MIMEType = req.MimeType

	if req.PostForm != nil {
		post.Params = mapToHarNVP(req.PostForm)
	} else {
		post.Text = req.Body
	}

	return post
}

// mapToHarNVP ranges over a map[string][]string and returns a slice of harNVP.
// For each key in the map, an instance of harNVP will be created for each element of
// the value slice.
func mapToHarNVP(m map[string][]string) []harNVP {
	nvps := []harNVP{}
	for n, vals := range m {
		for _, v := range vals {
			nvps = append(nvps, harNVP{Name: n, Value: v})
		}
	}
	return nvps
}

// ensureAbsoluteURL makes sure that the URL in a given Request is an absolute URL by setting the scheme to "http"
// and the host to a string built from the server IP address and Port provided in the Request. If scheme and host
// are already set, there will be no change.
func ensureAbsoluteURL(req *Request) {
	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}

	if req.URL.Host == "" {
		var host strings.Builder
		fmt.Fprintf(&host, "%v", req.ServerAddr)
		if req.ServerPort != "" {
			fmt.Fprintf(&host, ":%v", req.ServerPort)
		}
		req.URL.Host = host.String()
	}
}

// HAR FORMAT DEFINITION STRUCTS

// har is the root of a HTTP Archive (HAR) file.
type har struct {
	Log harLog `json:"log"`
}

// harLog is the topmost object in a HAR file.
type harLog struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Entries []harEntry `json:"entries"`
	Comment string     `json:"comment,omitempty"`
}

// harCreator is the object used for Creator and Browser entries at the harLog level.
type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Comment string `json:"comment,omitempty"`
}

// harEntry is the object for the Entries entry at the harLog level.
type harEntry struct {
	Pageref         string         `json:"pageref,omitempty"`
	StartedDateTime string         `json:"startedDateTime"`
	Time            int64          `json:"time"`
	Request         harRequest     `json:"request"`
	Response        harResponse    `json:"response"`
	Cache           harCache       `json:"cache,omitempty"`
	Timings         harEntryTiming `json:"timings"`
	ServerIPAddress string         `json:"serverIPAddress,omitempty"`
	Connection      string         `json:"connection,omitempty"`
	Comment         string         `json:"comment,omitempty"`
}

// harRequest is the object for the Request entry at the harEntry level.
type harRequest struct {
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []harCookie `json:"cookies"`
	Headers     []harNVP    `json:"headers"`
	QueryString []harNVP    `json:"queryString"`
	PostData    harPOST     `json:"postData,omitempty"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
	Comment     string      `json:"comment,omitempty"`
}

// harResponse is the object for the Response entry at the harEntry level.
type harResponse struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []harCookie `json:"cookies"`
	Headers     []harNVP    `json:"headers"`
	Content     harContent  `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
	Comment     string      `json:"comment,omitempty"`
}

// harCookie is the object for the Cookies entry at the harRequest and harResponse levels.
type harCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Path     string `json:"path,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Expires  string `json:"expires,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// harPOST is the object for the PostData entry at the harRequest level.
type harPOST struct {
	MIMEType string   `json:"mimeType"`
	Params   []harNVP `json:"params,omitempty"` // Mutually exclusive with Text
	Text     string   `json:"text,omitempty"`   // Mutually exclusive with Params
	Comment  string   `json:"comment,omitempty"`
}

// harNVP is a name-value pair and is used at harRequest, harResponse, and harPOST levels.
type harNVP struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
}

// harContent is the object for the Content entry at the harResponse level.
type harContent struct {
	Size        int64  `json:"size"`
	Compression int    `json:"compression,omitempty"`
	MimeType    string `json:"mimeType"`
	Text        string `json:"text,omitempty"`
	Encoding    string `json:"encoding,omitempty"`
	Comment     string `json:"comment,omitempty"`
}

// harCache is the object for the Cache entry at the harEntry level.
type harCache struct {
	// This part of the HAR specification depends on browserish things, but we will
	// include an empty entry to denote intentional omission.
}

// harEntryTiming is the object for the Timings entry at the harEntry level.
type harEntryTiming struct {
	Blocked int64  `json:"blocked,omitempty"`
	DNS     int64  `json:"dns,omitempty"`
	Connect int64  `json:"connect,omitempty"`
	Send    int64  `json:"send"`
	Wait    int64  `json:"wait"`
	Receive int64  `json:"receive"`
	SSL     int64  `json:"ssl,omitempty"`
	Comment string `json:"comment,omitempty"`
}
