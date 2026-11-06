package rest

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"booking/internal/handler/rest/gen"
)

// NewHTTPHandler builds the complete HTTP surface. Every request gets a deadline
// so that a request waiting for a lock or a connection cannot wait forever.
//
// The order of the layers matters: logging is outermost so it records the
// status recovery produces for a panic, and recovery wraps everything else.
func NewHTTPHandler(handler *Handler, requestTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	gen.HandlerWithOptions(handler, gen.StdHTTPServerOptions{
		BaseRouter:       mux,
		ErrorHandlerFunc: handler.writeParamError,
	})

	var root http.Handler = handler.routeErrors(mux)
	root = withTimeout(root, requestTimeout)
	root = handler.withRecovery(root)
	root = handler.withRequestLog(root, mux)

	return root
}

// routeErrors answers in JSON when no route matches. The mux decides between 404
// and 405 but replies in plain text, so its reply is observed and replaced.
func (h *Handler) routeErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)

			return
		}

		observed := &discardWriter{header: http.Header{}, status: http.StatusOK}
		mux.ServeHTTP(observed, r)

		if observed.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", observed.header.Get("Allow"))
			h.writeProblem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "this method is not allowed for this path", nil)

			return
		}

		h.writeProblem(w, r, http.StatusNotFound, codeNotFound, "no such path", nil)
	})
}

func withTimeout(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			if recovered == http.ErrAbortHandler { //nolint:errorlint,err113 // The server compares this sentinel the same way.
				panic(recovered)
			}

			h.log.ErrorContext(r.Context(), "panic while handling request", slog.Any("panic", recovered))
			h.writeProblem(w, r, http.StatusInternalServerError, codeInternal, "internal server error", nil)
		}()

		next.ServeHTTP(w, r)
	})
}

func (h *Handler) withRequestLog(next http.Handler, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		_, route := mux.Handler(r)
		h.log.InfoContext(r.Context(), "request",
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(started)),
		)
	})
}

// statusRecorder remembers the status written through it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// discardWriter captures a reply's status and headers and drops its body.
type discardWriter struct {
	header http.Header
	status int
}

func (w *discardWriter) Header() http.Header {
	return w.header
}

func (w *discardWriter) WriteHeader(status int) {
	w.status = status
}

func (w *discardWriter) Write(body []byte) (int, error) {
	return len(body), nil
}
