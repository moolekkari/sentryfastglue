package sentryfastglue

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const valuesKey = "sentry"

type Handler struct {
	repanic         bool
	waitForDelivery bool
	timeout         time.Duration
}

type Options struct {
	// Repanic configures whether Sentry should repanic after recovery, in most cases it should be set to false,
	// as fasthttp doesn't include it's own Recovery handler.
	Repanic bool
	// WaitForDelivery configures whether you want to block the request before moving forward with the response.
	// Because fasthttp doesn't include it's own Recovery handler, it will restart the application,
	// and event won't be delivered otherwise.
	WaitForDelivery bool
	// Timeout for the event delivery requests.
	Timeout time.Duration
}

// New returns a struct that provides Handle method
// that satisfy fasthttp.RequestHandler interface.
func New(options Options) *Handler {
	timeout := options.Timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	return &Handler{
		repanic:         options.Repanic,
		timeout:         timeout,
		waitForDelivery: options.WaitForDelivery,
	}
}

// Handle wraps fasthttp.RequestHandler and recovers from caught panics.
func (h *Handler) Handle(handler fastglue.FastRequestHandler) fastglue.FastRequestHandler {
	return func(ctx *fastglue.Request) error {
		// Unlike for other integrations, we don't support getting an existing
		// hub from the current request context because fasthttp doesn't use the
		// standard net/http.Request and because fasthttp.RequestCtx implements
		// context.Context but requires string keys.
		hub := sentry.CurrentHub().Clone()
		if hub == nil {
			hub = sentry.GetHubFromContext(ctx.RequestCtx)
		}
		scope := hub.Scope()
		scope.SetRequest(convert(ctx))
		scope.SetRequestBody(ctx.RequestCtx.Request.Body())
		ctx.RequestCtx.SetUserValue(valuesKey, hub)
		defer h.recoverWithSentry(hub, ctx)
		return handler(ctx)
	}
}

func (h *Handler) recoverWithSentry(hub *sentry.Hub, req *fastglue.Request) {
	if err := recover(); err != nil {
		eventID := hub.RecoverWithContext(
			context.WithValue(req.RequestCtx, sentry.RequestContextKey, req),
			err,
		)
		if eventID != nil && h.waitForDelivery {
			hub.Flush(h.timeout)
		}
		if h.repanic {
			panic(err)
		}
	}
}

// GetHubFromContext retrieves attached *sentry.Hub instance from fasthttp.RequestCtx.
func GetHubFromContext(ctx *fasthttp.RequestCtx) *sentry.Hub {
	hub := ctx.UserValue(valuesKey)
	if hub, ok := hub.(*sentry.Hub); ok {
		return hub
	}
	return nil
}

func convert(ctx *fastglue.Request) *http.Request {
	defer func() {
		if err := recover(); err != nil {
			sentry.Logger.Printf("%v", err)
		}
	}()

	r := new(http.Request)

	r.Method = string(ctx.RequestCtx.Method())
	uri := ctx.RequestCtx.URI()
	// Ignore error.
	r.URL, _ = url.Parse(fmt.Sprintf("%s://%s%s", uri.Scheme(), uri.Host(), uri.Path()))

	// Headers
	r.Header = make(http.Header)
	r.Header.Add("Host", string(ctx.RequestCtx.Host()))
	ctx.RequestCtx.Request.Header.VisitAll(func(key, value []byte) {
		r.Header.Add(string(key), string(value))
	})
	r.Host = string(ctx.RequestCtx.Host())

	// Cookies
	ctx.RequestCtx.Request.Header.VisitAllCookie(func(key, value []byte) {
		r.AddCookie(&http.Cookie{Name: string(key), Value: string(value)})
	})

	// Env
	r.RemoteAddr = ctx.RequestCtx.RemoteAddr().String()

	// QueryString
	r.URL.RawQuery = string(ctx.RequestCtx.URI().QueryString())

	// Body
	r.Body = io.NopCloser(bytes.NewReader(ctx.RequestCtx.Request.Body()))

	return r
}
