//go:build unit

package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type devinAdminStub struct {
	service.AdminService
	account *service.Account
	created int
	update  *service.UpdateAccountInput
}

func (s *devinAdminStub) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.account, nil
}
func (s *devinAdminStub) CreateAccount(_ context.Context, in *service.CreateAccountInput) (*service.Account, error) {
	s.created++
	s.account = &service.Account{ID: 12, Name: in.Name, Platform: in.Platform, Type: in.Type, Credentials: in.Credentials, Extra: in.Extra}
	return s.account, nil
}
func (s *devinAdminStub) UpdateAccount(_ context.Context, _ int64, in *service.UpdateAccountInput) (*service.Account, error) {
	s.update = in
	return s.account, nil
}
func (s *devinAdminStub) ClearAccountError(context.Context, int64) (*service.Account, error) {
	return s.account, nil
}

type devinRepoStub struct{ service.AccountRepository }

func (*devinRepoStub) FindByExtraField(context.Context, string, any) ([]service.Account, error) {
	return nil, nil
}
func TestDevinCommitIsIdempotentAndReauthPreservesSettings(t *testing.T) {
	for _, reauth := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "reauthorize"}[reauth], func(t *testing.T) {
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			defer rdb.Close()
			repo := &devinRepoStub{}
			d := service.NewDevinService(repo, nil, nil, &config.Config{}, devin.NewRedisSessionStore(rdb))
			a := &devinAdminStub{account: &service.Account{ID: 12, Name: "old", Platform: service.PlatformDevin, Status: service.StatusError, Extra: map[string]any{"billing_custom": "retain"}, Credentials: map[string]any{"base_url": "https://server.codeium.com", "api_key": "cog_old"}}}
			h := NewDevinHandler(d, a, repo)
			v := &service.DevinAuthSession{ID: "session", Owner: 7, Status: "ready", Credentials: map[string]any{"access_token": "devin-session-token$new", "auth_mode": "cli"}, Models: []string{"swe-2-max"}, ExpiresAt: time.Now().Add(time.Minute).Unix()}
			if reauth {
				v.TargetAccountID = 12
			}
			if err := d.SaveAuth(context.Background(), v); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/commit", strings.NewReader(`{"name":"test","group_ids":[4],"concurrency":2}`))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Params = gin.Params{{Key: "id", Value: "session"}}
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
				h.Commit(c)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "devin-session-token$") {
					t.Fatal("commit exposed credentials")
				}
			}
			if reauth {
				if a.created != 0 || a.update == nil {
					t.Fatal("reauth created an account")
				}
				if a.update.Extra["billing_custom"] != "retain" || a.update.Credentials["base_url"] != "https://server.codeium.com" {
					t.Fatal("existing metadata lost")
				}
				if a.update.GroupIDs != nil || a.update.Priority != nil || a.update.RateMultiplier != nil {
					t.Fatal("reauth changed account settings")
				}
				if a.update.Credentials["api_key"] != "" {
					t.Fatal("old key retained after switching identity")
				}
			} else if a.created != 1 {
				t.Fatal("repeated commit created accounts", a.created)
			}
		})
	}
}
