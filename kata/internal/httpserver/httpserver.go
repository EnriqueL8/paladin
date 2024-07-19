// Copyright © 2023 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httpserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-common/pkg/log"
	"github.com/kaleido-io/paladin/kata/internal/confutil"
	"github.com/kaleido-io/paladin/kata/internal/msgs"
	"github.com/kaleido-io/paladin/kata/internal/types"
	"github.com/kaleido-io/paladin/kata/pkg/tls"
)

type Server interface {
	Start() error
	Stop()
}

type httpServer struct {
	ctx             context.Context
	cancelCtx       func()
	description     string
	listener        net.Listener
	httpServer      *http.Server
	httpServerDone  chan error
	shutdownTimeout time.Duration
	started         bool
}

func NewServer(ctx context.Context, description string, conf *Config, handler http.Handler) (_ Server, err error) {

	s := &httpServer{
		description:     description,
		httpServerDone:  make(chan error),
		shutdownTimeout: confutil.DurationMin(conf.ShutdownTimeout, 0, *HTTPDefaults.ShutdownTimeout),
	}
	s.ctx, s.cancelCtx = context.WithCancel(ctx)

	listenAddr := fmt.Sprintf("%s:%d", confutil.StringNotEmpty(conf.Address, *HTTPDefaults.Address), confutil.IntMin(conf.Port, 0, *HTTPDefaults.Port))
	if s.listener, err = net.Listen("tcp", listenAddr); err != nil {
		return nil, i18n.WrapError(ctx, err, msgs.MsgJSONRPCServerStartFailed, listenAddr)
	}
	log.L(ctx).Infof("%s server listening on %s", description, s.listener.Addr())

	tlsConfig, err := tls.BuildTLSConfig(ctx, &conf.TLS, tls.ServerType)
	if err != nil {
		return nil, err
	}

	maxRequestTimeout := confutil.DurationMin(conf.MaxRequestTimeout, 1*time.Second, *HTTPDefaults.MaxRequestTimeout)
	defaultRequestTimeout := confutil.DurationMin(conf.DefaultRequestTimeout, 1*time.Second, *HTTPDefaults.DefaultRequestTimeout)
	readTimeout := confutil.DurationMin(conf.ReadTimeout, maxRequestTimeout+1*time.Second, "0")
	writeTimeout := confutil.DurationMin(conf.WriteTimeout, maxRequestTimeout+1*time.Second, "0")

	handler = s.withLogAndTimeout(handler, defaultRequestTimeout, maxRequestTimeout)
	handler = WrapCorsIfEnabled(ctx, handler, &conf.CORS)

	log.L(ctx).Debugf("%s server timeouts: read=%s write=%s request=%s", description, readTimeout, writeTimeout, maxRequestTimeout)
	s.httpServer = &http.Server{
		Handler:           handler,
		WriteTimeout:      writeTimeout,
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: readTimeout, // safe for this to always be the read timeout - should be short
		TLSConfig:         tlsConfig,
		ConnContext: func(newCtx context.Context, c net.Conn) context.Context {
			l := log.L(ctx).WithField("req", types.ShortID())
			newCtx = log.WithLogger(newCtx, l)
			l.Debugf("New %s connection: remote=%s local=%s", description, c.RemoteAddr().String(), c.LocalAddr().String())
			return newCtx
		},
	}

	return s, err
}

func (s *httpServer) runAPIServer() {
	err := s.httpServer.Serve(s.listener)
	s.httpServerDone <- err
}

func (s *httpServer) calcRequestTimeout(req *http.Request, defaultTimeout, maxTimeout time.Duration) time.Duration {
	// Configure a server-side timeout on each request, to try and avoid cases where the API requester
	// times out, and we continue to churn indefinitely processing the request.
	// Long-running processes should be dispatched asynchronously (API returns 202 Accepted asap),
	// and the caller can either listen on the websocket for updates, or poll the status of the affected object.
	// This is dependent on the context being passed down through to all blocking operations down the stack
	// (while avoiding passing the context to asynchronous tasks that are dispatched as a result of the request)
	reqTimeout := defaultTimeout
	reqTimeoutHeader := req.Header.Get("Request-Timeout")
	if reqTimeoutHeader != "" {
		var customTimeout time.Duration
		timeoutInt, err := strconv.ParseInt(reqTimeoutHeader, 10, 32)
		if err == nil {
			customTimeout = (time.Duration)(timeoutInt) * time.Second
		} else {
			customTimeout, err = time.ParseDuration(reqTimeoutHeader)
		}
		if err != nil {
			log.L(req.Context()).Warnf("Invalid Request-Timeout header '%s': %s", reqTimeoutHeader, err)
		} else {
			reqTimeout = customTimeout
			if reqTimeout > maxTimeout {
				reqTimeout = maxTimeout
			}
		}
	}
	return reqTimeout
}

type logCapture struct {
	status int
	res    http.ResponseWriter
}

func (lc *logCapture) Header() http.Header {
	return lc.res.Header()
}

func (lc *logCapture) Write(data []byte) (int, error) {
	return lc.res.Write(data)
}

func (lc *logCapture) WriteHeader(statusCode int) {
	lc.status = statusCode
	lc.res.WriteHeader(statusCode)
}

func (s *httpServer) withLogAndTimeout(handler http.Handler, defaultRequestTimeout, maxRequestTimeout time.Duration) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		startTime := time.Now()

		ctx, cancel := context.WithTimeout(req.Context(), s.calcRequestTimeout(req, defaultRequestTimeout, maxRequestTimeout))
		defer cancel()
		req = req.WithContext(ctx)

		log.L(ctx).Debugf("--> %s %s (%s)", req.Method, req.URL.Path, s.description)

		lc := &logCapture{res: res, status: http.StatusOK}
		handler.ServeHTTP(lc, req)

		durationMS := float64(time.Since(startTime)) / float64(time.Millisecond)
		log.L(ctx).Infof("<-- %s %s [%d] (%.2fms)", req.Method, req.URL.Path, lc.status, durationMS)
	})
}

func (s *httpServer) Start() error {
	s.started = true
	go s.runAPIServer()
	return nil
}

func (s *httpServer) Stop() {
	if s.started {
		log.L(s.ctx).Infof("%s server shutting down", s.description)
		shutdownStarted := time.Now()
		gracefulShutdown := make(chan struct{})
		go func() {
			defer close(gracefulShutdown)
			_ = s.httpServer.Shutdown(s.ctx)
		}()
		if s.started {
			select {
			case <-time.After(s.shutdownTimeout):
				log.L(s.ctx).Warnf("%s server terminating after waiting %s for shutdown", s.description, time.Since(shutdownStarted))
				_ = s.httpServer.Close()
			case <-gracefulShutdown:
				return
			}
		}
		s.cancelCtx()
		err := <-s.httpServerDone
		log.L(s.ctx).Infof("%s server ended (err=%v)", s.description, err)
		s.started = false
	}
}
