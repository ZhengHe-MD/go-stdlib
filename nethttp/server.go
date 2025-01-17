// +build go1.7

package nethttp

import (
	"context"
	"net/http"
	"net/url"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

type mwOptions struct {
	opNameFunc    func(r *http.Request) string
	spanFilter    func(r *http.Request) bool
	// NOTE: keep spanObserver for compatibility
	spanObserver  func(span opentracing.Span, r *http.Request)
	spanOnStart   func(ctx context.Context, span opentracing.Span, r *http.Request) context.Context
	spanOnFinish  func(ctx context.Context, span opentracing.Span, r *http.Request) context.Context
	urlTagFunc    func(u *url.URL) string
	componentName string
}

// MWOption controls the behavior of the Middleware.
type MWOption func(*mwOptions)

// OperationNameFunc returns a MWOption that uses given function f
// to generate operation name for each server-side span.
func OperationNameFunc(f func(r *http.Request) string) MWOption {
	return func(options *mwOptions) {
		options.opNameFunc = f
	}
}

// MWComponentName returns a MWOption that sets the component name
// for the server-side span.
func MWComponentName(componentName string) MWOption {
	return func(options *mwOptions) {
		options.componentName = componentName
	}
}

// MWSpanFilter returns a MWOption that filters requests from creating a span
// for the server-side span.
// Span won't be created if it returns false.
func MWSpanFilter(f func(r *http.Request) bool) MWOption {
	return func(options *mwOptions) {
		options.spanFilter = f
	}
}

// MWSpanObserver returns a MWOption that observe the span
// for the server-side span.
func MWSpanObserver(f func(span opentracing.Span, r *http.Request)) MWOption {
	return func(options *mwOptions) {
		options.spanObserver = f
	}
}

// MWSpanOnStart returns a MWOption that observe the span right after the span started
// for the server-side span.
func MWSpanOnStart(f func(ctx context.Context, span opentracing.Span, r *http.Request) context.Context) MWOption {
	return func(options *mwOptions) {
		options.spanOnStart = f
	}
}

// MWSpanOnFinish returns MWOption that observe the span right before the span finished
// for the server-side span.
func MWSpanOnFinish(f func(ctx context.Context, span opentracing.Span, r *http.Request) context.Context) MWOption {
	return func(options *mwOptions) {
		options.spanOnFinish = f
	}
}

func noopObserver(span opentracing.Span, r *http.Request) {}
func noopHook(ctx context.Context, span opentracing.Span, r *http.Request) context.Context {return ctx}

// MWURLTagFunc returns a MWOption that uses given function f
// to set the span's http.url tag. Can be used to change the default
// http.url tag, eg to redact sensitive information.
func MWURLTagFunc(f func(u *url.URL) string) MWOption {
	return func(options *mwOptions) {
		options.urlTagFunc = f
	}
}

// Middleware wraps an http.Handler and traces incoming requests.
// Additionally, it adds the span to the request's context.
//
// By default, the operation name of the spans is set to "HTTP {method}".
// This can be overriden with options.
//
// Example:
// 	 http.ListenAndServe("localhost:80", nethttp.Middleware(tracer, http.DefaultServeMux))
//
// The options allow fine tuning the behavior of the middleware.
//
// Example:
//   mw := nethttp.Middleware(
//      tracer,
//      http.DefaultServeMux,
//      nethttp.OperationNameFunc(func(r *http.Request) string {
//	        return "HTTP " + r.Method + ":/api/customers"
//      }),
//      nethttp.MWSpanObserver(func(sp opentracing.Span, r *http.Request) {
//			sp.SetTag("http.uri", r.URL.EscapedPath())
//		}),
//   )
func Middleware(tr opentracing.Tracer, h http.Handler, options ...MWOption) http.Handler {
	return MiddlewareFunc(tr, h.ServeHTTP, options...)
}

// MiddlewareFunc wraps an http.HandlerFunc and traces incoming requests.
// It behaves identically to the Middleware function above.
//
// Example:
//   http.ListenAndServe("localhost:80", nethttp.MiddlewareFunc(tracer, MyHandler))
func MiddlewareFunc(tr opentracing.Tracer, h http.HandlerFunc, options ...MWOption) http.HandlerFunc {
	opts := mwOptions{
		opNameFunc: func(r *http.Request) string {
			return "HTTP " + r.Method
		},
		spanFilter:   func(r *http.Request) bool { return true },
		spanObserver: noopObserver,
		spanOnStart:  noopHook,
		spanOnFinish: noopHook,
		urlTagFunc: func(u *url.URL) string {
			return u.String()
		},
	}
	for _, opt := range options {
		opt(&opts)
	}
	fn := func(w http.ResponseWriter, r *http.Request) {
		if !opts.spanFilter(r) {
			h(w, r)
			return
		}
		spanCtx, _ := tr.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(r.Header))
		sp := tr.StartSpan(opts.opNameFunc(r), ext.RPCServerOption(spanCtx))
		ext.HTTPMethod.Set(sp, r.Method)
		ext.HTTPUrl.Set(sp, opts.urlTagFunc(r.URL))
		opts.spanObserver(sp, r)
		ctx := r.Context()
		ctx = opts.spanOnStart(ctx, sp, r)

		// set component name, use "net/http" if caller does not specify
		componentName := opts.componentName
		if componentName == "" {
			componentName = defaultComponentName
		}
		ext.Component.Set(sp, componentName)

		sct := &statusCodeTracker{ResponseWriter: w}
		r = r.WithContext(opentracing.ContextWithSpan(r.Context(), sp))

		defer func() {
			ext.HTTPStatusCode.Set(sp, uint16(sct.status))
			if sct.status >= http.StatusInternalServerError || !sct.wroteheader {
				ext.Error.Set(sp, true)
			}
			opts.spanOnFinish(ctx, sp, r)
			sp.Finish()
		}()

		h(sct.wrappedResponseWriter(), r)
	}
	return http.HandlerFunc(fn)
}
