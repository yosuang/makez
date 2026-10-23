package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

func TestSSEFlushAndClientDisconnect(t *testing.T) {
	done := make(chan string, 1)
	s := &Server{Metrics: newTestMetrics(t)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reason, _ := streamEvents(r.Context(), w, 0, time.Second, func(ctx context.Context, _ int64) (store.Task, []store.Event, error) {
			<-ctx.Done()
			return store.Task{}, nil, ctx.Err()
		}, s)
		done <- reason
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("headers did not flush: %v", err)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("wrong SSE content type")
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
	select {
	case reason := <-done:
		if reason != "canceled" {
			t.Fatal(reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not cancel")
	}
}

func TestSSEDrainsMoreThan500EventsWithoutPollingDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 800*time.Millisecond)
	defer cancel()
	fetch := func(_ context.Context, after int64) (store.Task, []store.Event, error) {
		events := []store.Event{}
		for n := after + 1; n <= min(after+store.EventPageSize, 1201); n++ {
			events = append(events, store.Event{Seq: n, Payload: json.RawMessage(`{"text":"ok"}`)})
		}
		return store.Task{State: core.TaskSucceeded}, events, nil
	}
	rec := httptest.NewRecorder()
	reason, err := streamEvents(ctx, rec, 100, time.Second, fetch, &Server{})
	if err != nil || reason != "terminal" {
		t.Fatalf("reason=%s err=%v", reason, err)
	}
	if strings.Count(rec.Body.String(), "event: log\n") != 1101 || strings.Contains(rec.Body.String(), "id: 100\n") || !strings.Contains(rec.Body.String(), "id: 1201\n") {
		t.Fatal("cursor replay lost or duplicated events")
	}
}

func TestSSESlowClientAndBlockedWriteCancellation(t *testing.T) {
	for _, cancelWrite := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelWrite), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan string, 1)
			started := make(chan struct{})
			payload, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", 60<<10)})
			fetch := func(context.Context, int64) (store.Task, []store.Event, error) {
				events := make([]store.Event, store.EventPageSize)
				for i := range events {
					events[i] = store.Event{Seq: int64(i + 1), Payload: payload}
				}
				return store.Task{State: core.TaskRunning}, events, nil
			}
			timeout := 100 * time.Millisecond
			if cancelWrite {
				timeout = time.Minute
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				reason, _ := streamEvents(ctx, w, 0, timeout, fetch, &Server{})
				done <- reason
			}))
			defer server.Close()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			}()
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetReadBuffer(1024)
			}
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			<-started
			if cancelWrite {
				time.AfterFunc(50*time.Millisecond, cancel)
			}
			select {
			case reason := <-done:
				want := "write_timeout"
				if cancelWrite {
					want = "canceled"
				}
				if reason != want {
					t.Fatalf("reason=%s want=%s", reason, want)
				}
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("blocked write did not end")
			}
		})
	}
}

func TestSSEAlreadyCanceledIsNotAWriteFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reason, err := streamEvents(ctx, httptest.NewRecorder(), 0, time.Second, func(context.Context, int64) (store.Task, []store.Event, error) {
		t.Fatal("canceled stream fetched data")
		return store.Task{}, nil, nil
	}, &Server{})
	if err != nil || reason != "canceled" {
		t.Fatalf("reason=%s err=%v", reason, err)
	}
}
