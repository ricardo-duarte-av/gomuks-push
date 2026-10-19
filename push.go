// gomuks/push - An FCM push gateway for gomuks android.
// Copyright (C) 2024-2026 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"go.mau.fi/util/exerrors"
	"go.mau.fi/util/exhttp"
	"go.mau.fi/util/exmaps"
	"go.mau.fi/util/exzerolog"
	"go.mau.fi/util/ptr"
	"go.mau.fi/util/requestlog"
	"go.mau.fi/zeroconfig"
	"google.golang.org/api/option"
)

var fcmPackageName = os.Getenv("FCM_PACKAGE_NAME")
var fcmPackageNameWhitelist = exmaps.NewSetWithItems(strings.Split(os.Getenv("FCM_PACKAGE_NAME_WHITELIST"), ","))
var fcmClient *messaging.Client

// sendFCM is the seam the push handler goes through to reach Firebase, so that the
// handler can be exercised without live credentials.
var sendFCM = func(ctx context.Context, msg *messaging.Message) (string, error) {
	return fcmClient.Send(ctx, msg)
}

func main() {
	log := exerrors.Must((&zeroconfig.Config{
		Writers: []zeroconfig.WriterConfig{{
			Type:     zeroconfig.WriterTypeStdout,
			Format:   zeroconfig.LogFormatPrettyColored,
			MinLevel: ptr.Ptr(zerolog.InfoLevel),
		}, {
			Type:   zeroconfig.WriterTypeFile,
			Format: zeroconfig.LogFormatJSON,
			FileConfig: zeroconfig.FileConfig{
				Filename:   "/var/log/gomuks-push.log",
				MaxSize:    100,
				MaxAge:     7,
				MaxBackups: 10,
			},
		}},
		MinLevel: ptr.Ptr(zerolog.TraceLevel),
	}).Compile())
	exzerolog.SetupDefaults(log)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_gomuks/push/fcm", instrumentHandler("push", handlePushProxy))
	mux.HandleFunc("GET /{$}", instrumentHandler("index", handleIndex))
	server := http.Server{
		Addr: fmt.Sprintf("%s:%s", os.Getenv("HOST"), cmp.Or(os.Getenv("PORT"), "8080")),
		Handler: exhttp.ApplyMiddleware(
			mux,
			hlog.NewHandler(*log),
			// Correlates the access log line (source IP, user agent) with the
			// FCM result line (owner, push token) for the same request.
			hlog.RequestIDHandler("request_id", "X-Request-ID"),
			requestlog.AccessLogger(requestlog.Options{TrustXForwardedFor: true}),
		),
		Protocols: &http.Protocols{},
	}
	server.Protocols.SetHTTP1(true)
	server.Protocols.SetUnencryptedHTTP2(true)
	ctx := log.WithContext(context.Background())
	app := exerrors.Must(firebase.NewApp(ctx, nil, option.WithAuthCredentialsFile(option.ServiceAccount, os.Getenv("FCM_CREDENTIALS_FILE"))))
	fcmClient = exerrors.Must(app.Messaging(ctx))
	metricsServer := serveMetrics(log)
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if metricsServer != nil {
			_ = metricsServer.Shutdown(ctx)
		}
		exerrors.PanicIfNotNil(server.Shutdown(ctx))
		cancel()
	}()
	log.Info().Str("listen_address", server.Addr).Msg("Starting server")
	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

//go:embed index.html
var indexPage []byte

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexPage)
}

type PushRequest struct {
	Token        string `json:"token"`
	Fid          string `json:"fid"`
	Owner        string `json:"owner"`
	Payload      []byte `json:"payload"`
	HighPriority bool   `json:"high_priority"`
	AppID        string `json:"app_id,omitempty"`
}

func (pr *PushRequest) ToFCM() (*messaging.Message, int) {
	appID := fcmPackageName
	if pr.AppID != "" {
		if !fcmPackageNameWhitelist.Has(pr.AppID) {
			return nil, http.StatusForbidden
		}
		appID = pr.AppID
	}
	return &messaging.Message{
		Data: map[string]string{
			"payload": base64.StdEncoding.EncodeToString(pr.Payload),
		},
		Android: &messaging.AndroidConfig{
			RestrictedPackageName: appID,
			Priority:              pr.GetPriority(),
		},
		//lint:ignore SA1019 push tokens are still in use
		Token: pr.Token,
		Fid:   pr.Fid,
	}, 0
}

func (pr *PushRequest) GetPriority() string {
	if pr.HighPriority {
		return "high"
	}
	return "normal"
}

const maxPayloadLength = 4000
const maxRequestLength = 5 * 1024

func handlePushProxy(w http.ResponseWriter, r *http.Request) {
	var req PushRequest
	if r.URL.Path != "/_gomuks/push/fcm" {
		rejectPush(w, rejectNotFound, http.StatusNotFound)
		return
	} else if r.ContentLength > maxRequestLength {
		rejectPush(w, rejectContentTooLarge, http.StatusRequestEntityTooLarge)
		return
	} else if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestLength)).Decode(&req); err != nil {
		rejectPush(w, rejectInvalidJSON, http.StatusBadRequest)
		return
	} else if base64.StdEncoding.EncodedLen(len(req.Payload)) > maxPayloadLength {
		rejectPush(w, rejectPayloadTooLarge, http.StatusRequestEntityTooLarge)
		return
	} else if len(req.Owner) == 0 || len(req.Owner) > 255 {
		rejectPush(w, rejectInvalidOwner, http.StatusBadRequest)
		return
	} else if len(req.Token) > 200 || len(req.Fid) > 200 ||
		(req.Fid != "" && req.Token != "") || (req.Fid == "" && req.Token == "") {
		rejectPush(w, rejectInvalidTarget, http.StatusBadRequest)
		return
	}
	payload, stat := req.ToFCM()
	if stat != 0 {
		rejectPush(w, rejectAppIDNotAllowed, stat)
		return
	}

	pushPayloadBytes.Observe(float64(len(req.Payload)))
	start := time.Now()
	resp, err := sendFCM(r.Context(), payload)
	duration := time.Since(start)

	if err != nil {
		hlog.FromRequest(r).
			Err(err).
			Str("push_token", req.Token).
			Str("installation_id", req.Fid).
			Str("owner", req.Owner).
			Msg("Failed to send FCM request")
		if messaging.IsSenderIDMismatch(err) || messaging.IsUnregistered(err) {
			recordPush(req.Owner, req.GetPriority(), resultInvalidToken, duration)
			w.WriteHeader(http.StatusNotFound)
		} else if messaging.IsQuotaExceeded(err) {
			recordPush(req.Owner, req.GetPriority(), resultQuotaExceeded, duration)
			w.WriteHeader(http.StatusTooManyRequests)
		} else if messaging.IsInvalidArgument(err) {
			recordPush(req.Owner, req.GetPriority(), resultInvalidArgument, duration)
			w.WriteHeader(http.StatusBadRequest)
		} else {
			recordPush(req.Owner, req.GetPriority(), resultError, duration)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	hlog.FromRequest(r).
		Info().
		Str("push_token", req.Token).
		Str("installation_id", req.Fid).
		Str("message_id", resp).
		Str("app_id", payload.Android.RestrictedPackageName).
		Str("owner", req.Owner).
		Msg("Sent FCM request")
	recordPush(req.Owner, req.GetPriority(), resultSent, duration)
	w.Header().Set("Content-Length", strconv.Itoa(len(resp)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(resp))
}
