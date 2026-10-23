//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/proto"
)

type devinDisconnectWriter struct {
	gin.ResponseWriter
	onWrite  func()
	broken   bool
	attempts int
}

func (w *devinDisconnectWriter) Write(b []byte) (int, error) {
	w.attempts++
	w.onWrite()
	if w.broken {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(b)
}

func (w *devinDisconnectWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func TestDevinGatewayDrainsUsageAfterClientDisconnect(t *testing.T) {
	protocols := []struct{ name, body string }{
		{"responses", `{"model":"swe-2-max","input":"hello","stream":true}`},
		{"chat", `{"model":"swe-2-max","messages":[{"role":"user","content":"hello"}],"stream":true}`},
		{"anthropic", `{"model":"swe-2-max","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`},
	}
	for _, protocol := range protocols {
		for _, broken := range []bool{false, true} {
			mode := "request canceled"
			if broken {
				mode = "write failed"
			}
			t.Run(protocol.name+"/"+mode, func(t *testing.T) {
				d, _, _ := devinTestService(t)
				upstream := d.upstream.(*devinHTTP)
				original := upstream.handle
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				firstWrite := make(chan struct{})
				producerDone := make(chan error, 1)
				first, _ := proto.Marshal(&pb.GetChatMessageResponse{MessageId: "message", DeltaText: "hello"})
				final, _ := proto.Marshal(&pb.GetChatMessageResponse{
					Usage:      &pb.ModelUsageStats{InputTokens: 7, OutputTokens: 9, CacheReadTokens: 3, CacheWriteTokens: 2, BillingModelUid: "terminal-billing-model"},
					StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN,
				})
				upstream.handle = func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != devin.ChatPath {
						return original(r)
					}
					reader, writer := io.Pipe()
					go func() {
						defer writer.Close()
						_, err := writer.Write(devinTestFrame(0, first))
						if err == nil {
							select {
							case <-firstWrite:
							case <-time.After(2 * time.Second):
								err = errors.New("downstream never received content")
							}
						}
						if err == nil {
							err = r.Context().Err()
						}
						if err == nil {
							_, err = writer.Write(append(devinTestFrame(0, final), devinTestFrame(2, []byte(`{}`))...))
						}
						producerDone <- err
					}()
					response := devinReply(200, nil)
					response.Body = reader
					return response, nil
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
				var once sync.Once
				writer := &devinDisconnectWriter{ResponseWriter: c.Writer, broken: broken, onWrite: func() {
					once.Do(func() {
						if !broken {
							cancel()
						}
						close(firstWrite)
					})
				}}
				c.Writer = writer
				account := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "test"}}
				gateway := &OpenAIGatewayService{devin: d}
				result, err := gateway.forwardDevin(ctx, c, account, []byte(protocol.body), protocol.name, "session", "")
				if err != nil || result == nil {
					t.Fatalf("generation was interrupted before final usage: result=%+v err=%v", result, err)
				}
				if !result.ClientDisconnect || writer.attempts != 1 {
					t.Fatalf("disconnect was not retained or writes continued: disconnected=%v writes=%d", result.ClientDisconnect, writer.attempts)
				}
				if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 9 || result.Usage.CacheReadInputTokens != 3 || result.Usage.CacheCreationInputTokens != 2 || result.BillingModel != "terminal-billing-model" {
					t.Fatalf("terminal usage was lost: %+v", result)
				}
				select {
				case err := <-producerDone:
					if err != nil {
						t.Fatalf("upstream was canceled before usage delivery: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("upstream stream was not released")
				}
			})
		}
	}
}

func TestDevinStreamDisconnectDrainTimeoutCancelsUpstream(t *testing.T) {
	first, _ := proto.Marshal(&pb.GetChatMessageResponse{MessageId: "message", DeltaText: "hello"})
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(devinTestFrame(0, first))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &devin.Client{Do: http.DefaultClient.Do}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	disconnected := false
	decoder := devin.NewEventDecoder("swe-2-max", func(_ apicompat.ResponsesStreamEvent) error {
		disconnected = true
		return nil
	})
	var firstToken *int
	started := time.Now()
	err := consumeDevinChat(context.Background(), c, client, server.URL, "test", &pb.GetChatMessageRequest{}, decoder, true, &firstToken, &disconnected, 20*time.Millisecond, started)
	if err == nil || !strings.Contains(err.Error(), "data interval timeout") {
		t.Fatalf("stalled drain was not bounded: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("drain timeout did not stop promptly")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("timed-out drain did not cancel upstream HTTP request")
	}
}
