package podtransport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

type consoleHealthTestRoundTrip func(*http.Request) (*http.Response, error)

func (f consoleHealthTestRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConsoleHealthConnectionConvergenceRetainsRouteAndBudget(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, scenario := range []string{"converged", "host-unreachable", "network-unreachable", "exhausted", "foreign-route", "unready", "cancelled", "cancel-during-observation", "deadline-during-wait", "post", "root-get", "tls", "response", "read-error"} {
		t.Run(scenario, func(t *testing.T) {
			budget := 5 * time.Second
			if scenario == "deadline-during-wait" {
				budget = 50 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			method, path := "GET", "/health/live"
			if scenario == "post" {
				method, path = "POST", "/api/demo"
			}
			if scenario == "root-get" {
				path = "/"
			}
			req, _ := http.NewRequestWithContext(ctx, method, "https://saferus-console:8443"+path, nil)
			calls, observations := 0, 0
			client := &http.Client{Transport: consoleHealthTestRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if scenario == "tls" {
					return nil, &tls.CertificateVerificationError{Err: errors.New("private certificate detail")}
				}
				if scenario == "read-error" {
					return nil, &net.OpError{Op: "read", Err: syscall.ECONNRESET}
				}
				if scenario == "cancelled" {
					cancel()
				}
				if scenario == "response" || (scenario == "converged" || scenario == "host-unreachable" || scenario == "network-unreachable") && calls == 3 {
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable")), Header: http.Header{}}, nil
				}
				if scenario == "host-unreachable" {
					return nil, &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}
				}
				if scenario == "network-unreachable" {
					return nil, &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}
				}
				return nil, refused
			})}
			original := ConsoleRollout{PodUID: "original", ServiceIP: "10.96.0.2", ObservedAt: 1}
			response, err := consoleHealthRequest(ctx, client, req, original, func(context.Context) (ConsoleRollout, bool, error) {
				observations++
				current := original
				current.ObservedAt++
				if scenario == "foreign-route" {
					current.PodUID = "replacement"
				}
				if scenario == "cancel-during-observation" {
					cancel()
				}
				return current, scenario != "unready", nil
			})
			if response != nil {
				defer response.Body.Close()
			}
			switch scenario {
			case "converged", "host-unreachable", "network-unreachable":
				if err != nil || response == nil || response.StatusCode != 503 || calls != 3 || observations != 2 {
					t.Fatal("connection convergence altered response or scope", calls, observations, err)
				}
			case "exhausted":
				if err == nil || calls != 8 || observations != 7 {
					t.Fatal("connection attempt bound", calls, observations, err)
				}
			case "foreign-route", "unready", "cancel-during-observation":
				if !errors.Is(err, ErrTransport) || calls != 1 || observations != 1 {
					t.Fatal("route change authorized another request", calls, observations, err)
				}
			case "response":
				if err != nil || response == nil || calls != 1 || observations != 0 {
					t.Fatal("HTTP response was retried")
				}
			case "deadline-during-wait":
				if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || observations != 0 {
					t.Fatal("expired wait sent another request", calls, observations, err)
				}
			default:
				if err == nil || calls != 1 || observations != 0 {
					t.Fatal("non-connect outcome was retried", calls, observations, err)
				}
			}
		})
	}
}
