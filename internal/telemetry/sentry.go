package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/getsentry/sentry-go"
)

func initSentry(version, commit string) {
	if SentryDSN == "" {
		return
	}

	env := "production"
	if version == "dev" || version == "" {
		env = "development"
	}

	release := version
	if commit != "" && commit != "unknown" {
		short := commit
		if len(short) > 7 {
			short = short[:7]
		}
		release = version + "-" + short
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:            SentryDSN,
		Release:        release,
		Environment:    env,
		SendDefaultPII: false,
		BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
			for i := range event.Exception {
				event.Exception[i].Value = scrub(event.Exception[i].Value)
			}
			return event
		},
	})
	if err != nil {
		slog.Debug("telemetry: sentry init failed", "err", err)
	}
}

func reportToSentry(e Event) {
	if SentryDSN == "" || globalDeviceID == "" {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("cli.version", cliVersion)
		scope.SetTag("cli.os", runtime.GOOS)
		scope.SetTag("cli.arch", runtime.GOARCH)
		scope.SetTag("cli.event_type", e.EventType)
		scope.SetTag("device_id", globalDeviceID)

		if e.Email != "" {
			mac := hmac.New(sha256.New, []byte(globalDeviceID))
			mac.Write([]byte(e.Email))
			scope.SetUser(sentry.User{
				ID: fmt.Sprintf("%x", mac.Sum(nil)),
			})
		}

		scope.SetExtra("error_code", e.ErrorCode)

		msg := fmt.Sprintf("[%s] %s", e.EventType, e.ErrorCode)
		if e.ErrorMessage != "" {
			msg += ": " + e.ErrorMessage
		}
		sentry.CaptureMessage(msg)
	})
}

func flushSentry() {
	sentry.Flush(2 * time.Second)
}
