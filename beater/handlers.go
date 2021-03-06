package beater

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/subtle"
	"encoding/json"
	"expvar"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"

	lru "github.com/hashicorp/golang-lru"
	"github.com/pkg/errors"
	glob "github.com/ryanuber/go-glob"
	uuid "github.com/satori/go.uuid"
	"golang.org/x/time/rate"

	"github.com/elastic/apm-server/processor"
	perr "github.com/elastic/apm-server/processor/error"
	"github.com/elastic/apm-server/processor/healthcheck"
	"github.com/elastic/apm-server/processor/sourcemap"
	"github.com/elastic/apm-server/processor/transaction"
	"github.com/elastic/apm-server/utility"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/monitoring"
)

const (
	BackendTransactionsURL  = "/v1/transactions"
	FrontendTransactionsURL = "/v1/client-side/transactions"
	BackendErrorsURL        = "/v1/errors"
	FrontendErrorsURL       = "/v1/client-side/errors"
	HealthCheckURL          = "/healthcheck"
	SourcemapsURL           = "/v1/client-side/sourcemaps"

	rateLimitCacheSize       = 1000
	rateLimitBurstMultiplier = 2

	supportedHeaders = "Content-Type, Content-Encoding, Accept"
	supportedMethods = "POST, OPTIONS"
)

type ProcessorFactory func(*processor.Config) processor.Processor

type ProcessorHandler func(ProcessorFactory, *Config, reporter) http.Handler

type routeMapping struct {
	ProcessorHandler
	ProcessorFactory
}

var (
	serverMetrics  = monitoring.Default.NewRegistry("apm-server.server")
	requestCounter = monitoring.NewInt(serverMetrics, "requests.counter")
	responseValid  = monitoring.NewInt(serverMetrics, "response.valid")
	responseErrors = monitoring.NewInt(serverMetrics, "response.errors")

	errInvalidToken    = errors.New("invalid token")
	errForbidden       = errors.New("forbidden request")
	errPOSTRequestOnly = errors.New("only POST requests are supported")
	errTooManyRequests = errors.New("too many requests")
	errNoContent       = errors.New("no content")

	Routes = map[string]routeMapping{
		BackendTransactionsURL:  {backendHandler, transaction.NewProcessor},
		FrontendTransactionsURL: {frontendHandler, transaction.NewProcessor},
		BackendErrorsURL:        {backendHandler, perr.NewProcessor},
		FrontendErrorsURL:       {frontendHandler, perr.NewProcessor},
		HealthCheckURL:          {healthCheckHandler, healthcheck.NewProcessor},
		SourcemapsURL:           {sourcemapHandler, sourcemap.NewProcessor},
	}
)

func newMuxer(config *Config, report reporter) *http.ServeMux {
	mux := http.NewServeMux()
	for path, mapping := range Routes {
		logp.Info("Path %s added to request handler", path)
		mux.Handle(path, mapping.ProcessorHandler(mapping.ProcessorFactory, config, report))
	}

	if config.Expvar.isEnabled() {
		path := config.Expvar.Url
		logp.Info("Path %s added to request handler", path)
		mux.Handle(path, expvar.Handler())
	}
	return mux
}

func backendHandler(pf ProcessorFactory, config *Config, report reporter) http.Handler {
	return logHandler(
		authHandler(config.SecretToken,
			processRequestHandler(pf, nil, report, decodeLimitJSONData(config.MaxUnzippedSize))))
}

func frontendHandler(pf ProcessorFactory, config *Config, report reporter) http.Handler {
	smapper, err := config.Frontend.SmapMapper()
	if err != nil {
		logp.Err(err.Error())
	}
	prConfig := processor.Config{
		SmapMapper:          smapper,
		LibraryPattern:      regexp.MustCompile(config.Frontend.LibraryPattern),
		ExcludeFromGrouping: regexp.MustCompile(config.Frontend.ExcludeFromGrouping),
	}
	return logHandler(
		killSwitchHandler(config.Frontend.isEnabled(),
			ipRateLimitHandler(config.Frontend.RateLimit,
				corsHandler(config.Frontend.AllowOrigins,
					processRequestHandler(pf, &prConfig, report, decodeLimitJSONData(config.MaxUnzippedSize))))))
}

func sourcemapHandler(pf ProcessorFactory, config *Config, report reporter) http.Handler {
	smapper, err := config.Frontend.SmapMapper()
	if err != nil {
		logp.Err(err.Error())
	}
	return logHandler(
		killSwitchHandler(config.Frontend.isEnabled(),
			authHandler(config.SecretToken,
				processRequestHandler(pf, &processor.Config{SmapMapper: smapper}, report, sourcemap.DecodeSourcemapFormData))))
}

func healthCheckHandler(_ ProcessorFactory, _ *Config, _ reporter) http.Handler {
	return logHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sendStatus(w, r, http.StatusOK, nil)
		}))
}

type logContextKey string

var reqLoggerContextKey = logContextKey("requestLogger")

func logHandler(h http.Handler) http.Handler {
	logger := logp.NewLogger("request")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := uuid.NewV4()

		requestCounter.Inc()

		reqLogger := logger.With("request_id", reqID)

		lr := r.WithContext(
			context.WithValue(r.Context(), reqLoggerContextKey, reqLogger),
		)

		lw := utility.NewRecordingResponseWriter(w)

		h.ServeHTTP(lw, lr)

		reqLogger.Infow("handled request", "response_code", lw.Code,
			"method", r.Method, "URL", r.URL, "content_length", r.ContentLength,
			"remote_address", extractIP(r), "user_agent", r.Header.Get("User-Agent"))

		if lw.Code > 399 {
			responseErrors.Inc()
		} else {
			responseValid.Inc()
		}
	})

}

func killSwitchHandler(killSwitch bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if killSwitch {
			h.ServeHTTP(w, r)
		} else {
			sendStatus(w, r, http.StatusForbidden, errForbidden)
		}
	})
}

func ipRateLimitHandler(rateLimit int, h http.Handler) http.Handler {

	cache, _ := lru.New(rateLimitCacheSize)

	var deny = func(ip string) bool {
		if !cache.Contains(ip) {
			cache.Add(ip, rate.NewLimiter(rate.Limit(rateLimit), rateLimit*rateLimitBurstMultiplier))
		}
		var limiter, _ = cache.Get(ip)
		return !limiter.(*rate.Limiter).Allow()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deny(extractIP(r)) {
			sendStatus(w, r, http.StatusTooManyRequests, errTooManyRequests)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func extractIP(r *http.Request) string {
	var remoteAddr = func() string {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return ip
	}

	var forwarded = func() string {
		forwardedFor := r.Header.Get("X-Forwarded-For")
		client := strings.Split(forwardedFor, ",")[0]
		return strings.TrimSpace(client)
	}

	var real = func() string {
		return r.Header.Get("X-Real-IP")
	}

	if ip := real(); ip != "" {
		return ip
	}
	if ip := forwarded(); ip != "" {
		return ip
	}
	return remoteAddr()
}

func authHandler(secretToken string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAuthorized(r, secretToken) {
			sendStatus(w, r, http.StatusUnauthorized, errInvalidToken)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// isAuthorized checks the Authorization header. It must be in the form of:
//   Authorization: Bearer <secret-token>
// Bearer must be part of it.
func isAuthorized(req *http.Request, secretToken string) bool {
	// No token configured
	if secretToken == "" {
		return true
	}
	header := req.Header.Get("Authorization")
	parts := strings.Split(header, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(secretToken)) == 1
}

func corsHandler(allowedOrigins []string, h http.Handler) http.Handler {

	var isAllowed = func(origin string) bool {
		for _, allowed := range allowedOrigins {
			if glob.Glob(allowed, origin) {
				return true
			}
		}
		return false
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// origin header is always set by the browser
		origin := r.Header.Get("Origin")
		validOrigin := isAllowed(origin)

		if r.Method == "OPTIONS" {

			// setting the ACAO header is the way to tell the browser to go ahead with the request
			if validOrigin {
				// do not set the configured origin(s), echo the received origin instead
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}

			// tell browsers to cache response requestHeaders for up to 1 hour (browsers might ignore this)
			w.Header().Set("Access-Control-Max-Age", "3600")
			// origin must be part of the cache key so that we can handle multiple allowed origins
			w.Header().Set("Vary", "Origin")

			// required if Access-Control-Request-Method and Access-Control-Request-Headers are in the requestHeaders
			w.Header().Set("Access-Control-Allow-Methods", supportedMethods)
			w.Header().Set("Access-Control-Allow-Headers", supportedHeaders)

			w.Header().Set("Content-Length", "0")

			sendStatus(w, r, http.StatusOK, nil)

		} else if validOrigin {
			// we need to check the origin and set the ACAO header in both the OPTIONS preflight and the actual request
			w.Header().Set("Access-Control-Allow-Origin", origin)
			h.ServeHTTP(w, r)

		} else {
			sendStatus(w, r, http.StatusForbidden, errForbidden)
		}
	})
}

func processRequestHandler(pf ProcessorFactory, prConfig *processor.Config, report reporter, decode decoder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, err := processRequest(r, pf, prConfig, report, decode)
		sendStatus(w, r, code, err)
	})
}

func processRequest(r *http.Request, pf ProcessorFactory, prConfig *processor.Config, report reporter, decode decoder) (int, error) {
	processor := pf(prConfig)

	if r.Method != "POST" {
		return http.StatusMethodNotAllowed, errPOSTRequestOnly
	}

	data, err := decode(r)
	if err != nil {
		return http.StatusBadRequest, errors.Wrap(err, "while decoding")
	}

	if err = processor.Validate(data); err != nil {
		return http.StatusBadRequest, err
	}

	list, err := processor.Transform(data)
	if err != nil {
		return http.StatusBadRequest, err
	}

	if err = report(list); err != nil {
		return http.StatusServiceUnavailable, err
	}

	return http.StatusAccepted, nil
}

type decoder func(req *http.Request) (map[string]interface{}, error)

func decodeLimitJSONData(maxSize int64) decoder {
	return func(req *http.Request) (map[string]interface{}, error) {
		contentType := req.Header.Get("Content-Type")
		if contentType != "application/json" {
			return nil, fmt.Errorf("invalid content type: %s", req.Header.Get("Content-Type"))
		}

		reader := req.Body
		if reader == nil {
			return nil, errNoContent
		}

		switch req.Header.Get("Content-Encoding") {
		case "deflate":
			var err error
			reader, err = zlib.NewReader(reader)
			if err != nil {
				return nil, err
			}

		case "gzip":
			var err error
			reader, err = gzip.NewReader(reader)
			if err != nil {
				return nil, err
			}
		}
		v := make(map[string]interface{})
		if err := json.NewDecoder(http.MaxBytesReader(nil, reader, maxSize)).Decode(&v); err != nil {
			// If we run out of memory, for example
			return nil, errors.Wrap(err, "data read error")
		}
		return v, nil
	}
}

func sendStatus(w http.ResponseWriter, r *http.Request, code int, err error) {
	content_type := "text/plain; charset=utf-8"
	if acceptsJSON(r) {
		content_type = "application/json"
	}
	w.Header().Set("Content-Type", content_type)
	w.WriteHeader(code)

	if err == nil {
		return
	}

	logger, ok := r.Context().Value(reqLoggerContextKey).(*logp.Logger)
	if ok {
		logger.Errorw("error handling request", "error", err.Error())
	} else {
		logp.Err("error handling request:", err.Error())
	}

	if acceptsJSON(r) {
		sendJSON(w, map[string]interface{}{"error": err.Error()})
	} else {
		sendPlain(w, err.Error())
	}
}

func acceptsJSON(r *http.Request) bool {
	h := r.Header.Get("Accept")
	return strings.Contains(h, "*/*") || strings.Contains(h, "application/json")
}

func sendJSON(w http.ResponseWriter, msg map[string]interface{}) {
	buf, err := json.Marshal(msg)
	if err != nil {
		logp.Err("Error while generating a JSON error response: %v", err)
		return
	}

	w.Write(buf)
}

func sendPlain(w http.ResponseWriter, msg string) {
	w.Write([]byte(msg))
}
