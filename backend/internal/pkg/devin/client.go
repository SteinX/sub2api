// Package devin implements the credential-scoped Devin Cascade protocol.
package devin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultBaseURL = "https://server.codeium.com"
	CallbackURI    = "http://127.0.0.1:59653/callback"
	TokenURL       = "https://api.devin.ai/auth/cli/token"
	ChatPath       = "/exa.api_server_pb.ApiServerService/GetChatMessage"
	ModelsPath     = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"
	UserJWTPath    = "/exa.auth_pb.AuthService/GetUserJwt"
	AssignPath     = "/exa.api_server_pb.ApiServerService/AssignModel"
	MintPath       = "/exa.seat_management_pb.SeatManagementService/GetSelfDevinSessionToken"
	MaxFrameSize   = 64 << 20
)

// Error preserves protocol codes even when the HTTP response itself was 200.
type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string { return "Devin upstream: " + e.Code }
func IsUnauthenticated(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Code == "unauthenticated" || e.Status == 401)
}
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case "unauthenticated":
		return 401
	case "permission_denied":
		return 403
	case "invalid_argument":
		return 400
	case "resource_exhausted":
		return 429
	case "deadline_exceeded":
		return 504
	}
	if e.Status >= 400 {
		return e.Status
	}
	return 502
}

type Client struct {
	BaseURL string
	Do      func(*http.Request) (*http.Response, error)
	// ValidateURL applies the host application's egress policy to account-specific hosts.
	ValidateURL     func(string) (string, error)
	PreEventTimeout time.Duration
	IdleTimeout     time.Duration
}

func NormalizeToken(token string) string {
	token = strings.TrimSpace(token)
	if token != "" && !strings.HasPrefix(token, "devin-session-token$") {
		token = "devin-session-token$" + token
	}
	return token
}

func Metadata(token string) *pb.Metadata {
	return &pb.Metadata{
		ApiKey: NormalizeToken(token), IdeName: "devin-cli", IdeType: "chisel",
		IdeVersion: "3000.6.2", ExtensionName: "chisel", ExtensionVersion: "3000.6.2",
		Os: "linux", Locale: "en", DisableTelemetry: true,
	}
}

// Discovery uses the CLI's dev-channel identity and requests all catalog slots.
// Cascade gates model visibility on these fields, independently of chat identity.
func discoveryMetadata(token string) *pb.Metadata {
	return &pb.Metadata{
		ApiKey: NormalizeToken(token), IdeName: "chisel", IdeVersion: "0.0.0-dev",
		ExtensionName: "chisel", ExtensionVersion: "0.0.0-dev",
		Os: "linux", Locale: "en", DisableTelemetry: true,
		SupportedModelDisplays: []pb.DisplayOption{
			pb.DisplayOption_DISPLAY_OPTION_MODEL_ROUTER,
			pb.DisplayOption_DISPLAY_OPTION_QUICK_REVIEW,
			pb.DisplayOption_DISPLAY_OPTION_INTERNAL_DEFAULT,
			pb.DisplayOption_DISPLAY_OPTION_UNCLASSIFIED,
			pb.DisplayOption_DISPLAY_OPTION_NORMAL,
		},
	}
}

func (c *Client) request(ctx context.Context, base, path, contentType string, body []byte, token string) (*http.Response, error) {
	if c.ValidateURL != nil {
		var err error
		base, err = c.ValidateURL(base)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Accept-Encoding", "gzip")
	if contentType == "application/connect+proto" {
		req.Header.Set("Connect-Content-Encoding", "gzip")
	}
	if token != "" {
		req.SetBasicAuth(token, token)
	}
	if c.Do != nil {
		return c.Do(req)
	}
	return http.DefaultClient.Do(req)
}

func (c *Client) unary(ctx context.Context, base, path, token string, in, out proto.Message) error {
	b, err := proto.Marshal(in)
	if err != nil {
		return err
	}
	r, err := c.request(ctx, base, path, "application/proto", b, token)
	if err != nil {
		return err
	}
	defer func() { _ = r.Body.Close() }()
	b, err = io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
	if err != nil {
		return err
	}
	if len(b) > 8<<20 {
		return errors.New("devin unary response too large")
	}
	if r.StatusCode != 200 {
		return responseError(r.StatusCode, b)
	}
	return proto.Unmarshal(b, out)
}

func responseError(status int, b []byte) error {
	var v struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(b, &v)
	if v.Code == "" {
		switch status {
		case 401:
			v.Code = "unauthenticated"
		case 403:
			v.Code = "permission_denied"
		case 429:
			v.Code = "resource_exhausted"
		default:
			v.Code = "unavailable"
		}
	}
	return &Error{Code: v.Code, Status: status}
}

func (c *Client) Models(ctx context.Context, token string) ([]*pb.ClientModelConfig, error) {
	out := &pb.GetCliModelConfigsResponse{}
	err := c.unary(ctx, c.BaseURL, ModelsPath, token, &pb.GetCliModelConfigsRequest{Metadata: discoveryMetadata(token)}, out)
	if err != nil {
		return nil, err
	}
	models := make([]*pb.ClientModelConfig, 0, len(out.ClientModelConfigs))
	seen := map[string]bool{}
	for _, m := range out.ClientModelConfigs {
		uid := ModelID(m)
		if uid == "" || m.Disabled || seen[uid] {
			continue
		}
		display := m.GetModelInfo().GetDisplayOption()
		if display == pb.DisplayOption_DISPLAY_OPTION_QUICK_REVIEW || display == pb.DisplayOption_DISPLAY_OPTION_INTERNAL_DEFAULT {
			continue
		}
		seen[uid] = true
		models = append(models, m)
	}
	return models, nil
}

func ModelID(m *pb.ClientModelConfig) string {
	if m.ModelUid != "" {
		return m.ModelUid
	}
	if m.GetModelInfo().GetModelUid() != "" {
		return m.ModelInfo.ModelUid
	}
	return m.GetModelOrAlias().GetModel()
}

// Prepare resolves the account host and optional router before sending any chat content.
func (c *Client) Prepare(ctx context.Context, token string, req *pb.GetChatMessageRequest, router bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out := &pb.GetUserJwtResponse{}
	if err := c.unary(ctx, c.BaseURL, UserJWTPath, token, &pb.GetUserJwtRequest{Metadata: Metadata(token)}, out); err != nil {
		return "", err
	}
	if out.UserJwt == "" {
		return "", errors.New("devin returned no user JWT")
	}
	req.Metadata = Metadata(token)
	req.Metadata.UserJwt = out.UserJwt
	base := c.BaseURL
	if out.CustomApiServerUrl != "" {
		u, parseErr := url.Parse(out.CustomApiServerUrl)
		if parseErr != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", errors.New("devin returned an invalid account API host")
		}
		base = out.CustomApiServerUrl
	}
	if router {
		a := &pb.AssignModelResponse{}
		in := &pb.AssignModelRequest{Metadata: req.Metadata, ModelRouterUid: req.ChatModelUid, CascadeId: req.CascadeId}
		for i := len(req.ChatMessagePrompts) - 1; i >= 0; i-- {
			if req.ChatMessagePrompts[i].Source == pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_USER {
				in.ChatMessagePrompt = req.ChatMessagePrompts[i]
				break
			}
		}
		if err := c.unary(ctx, base, AssignPath, token, in, a); err != nil {
			return "", err
		}
		if a.GetAssignment().GetModelUid() == "" || a.GetAssignment().GetAssignmentJwt() == "" {
			return "", errors.New("Devin returned an incomplete model assignment")
		}
		req.ChatModelUid = a.Assignment.ModelUid
		req.ModelAssignmentJwt = proto.String(a.Assignment.AssignmentJwt)
	}
	return base, nil
}

func (c *Client) Chat(ctx context.Context, base, token string, in *pb.GetChatMessageRequest, emit func(*pb.GetChatMessageResponse) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	firstBudget, idleBudget := c.PreEventTimeout, c.IdleTimeout
	if firstBudget <= 0 {
		firstBudget = 3 * time.Minute
	}
	if idleBudget <= 0 {
		idleBudget = 45 * time.Minute
	}
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	var hasContent atomic.Bool
	interval := min(firstBudget/4, idleBudget/4, 10*time.Second)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				budget := firstBudget
				if hasContent.Load() {
					budget = idleBudget
				}
				if time.Since(time.Unix(0, lastProgress.Load())) > budget {
					cancel()
					return
				}
			}
		}
	}()
	data, err := proto.Marshal(in)
	if err != nil {
		return err
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err = zw.Write(data); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	data = make([]byte, 5)
	data[0] = 1
	binary.BigEndian.PutUint32(data[1:], uint32(zipped.Len()))
	data = append(data, zipped.Bytes()...)
	// The request frame is compressed independently of HTTP content encoding.
	r, err := c.request(ctx, base, ChatPath, "application/connect+proto", data, token)
	if err != nil {
		return err
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		return responseError(r.StatusCode, b)
	}
	for {
		flags, b, err := ReadFrame(r.Body)
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if flags&2 != 0 {
			var end struct {
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err = json.Unmarshal(b, &end); err != nil {
				return errors.New("invalid Devin trailer")
			}
			if end.Error != nil {
				return &Error{Code: end.Error.Code}
			}
			return nil
		}
		m := &pb.GetChatMessageResponse{}
		if err = proto.Unmarshal(b, m); err != nil {
			return err
		}
		if m.DeltaText != "" || m.DeltaThinking != "" || m.DeltaSignature != "" || len(m.DeltaToolCalls) > 0 {
			hasContent.Store(true)
			lastProgress.Store(time.Now().UnixNano())
		}
		if err = emit(m); err != nil {
			return err
		}
	}
}

// ReadFrame bounds both compressed and expanded payloads before decoding them.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxFrameSize {
		return 0, nil, errors.New("Devin frame too large")
	}
	if h[0]&^byte(3) != 0 {
		return 0, nil, errors.New("invalid Devin frame flags")
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	if h[0]&1 != 0 {
		z, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = z.Close() }()
		b, err = io.ReadAll(io.LimitReader(z, MaxFrameSize+1))
		if err != nil {
			return 0, nil, err
		}
		if len(b) > MaxFrameSize {
			return 0, nil, errors.New("expanded Devin frame too large")
		}
	}
	return h[0], b, nil
}

func (c *Client) Mint(ctx context.Context, key string) (string, error) {
	b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"api_key": key, "ide_name": "windsurf", "extension_name": "windsurf", "ide_version": "1.48.2", "extension_version": "1.48.2", "locale": "en", "os": "windows"}})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+MintPath, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Api-Key", key)
	do := c.Do
	if do == nil {
		do = http.DefaultClient.Do
	}
	r, err := do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Body.Close() }()
	b, err = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if r.StatusCode != 200 {
		return "", responseError(r.StatusCode, b)
	}
	var v struct {
		Token string `json:"sessionToken"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	if v.Token == "" {
		return "", errors.New("Devin returned no session token")
	}
	return NormalizeToken(v.Token), nil
}

func (c *Client) Exchange(ctx context.Context, code, verifier string) (string, error) {
	b, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	r, err := c.request(ctx, "https://api.devin.ai", "/auth/cli/token", "application/json", b, "")
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Body.Close() }()
	b, err = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if r.StatusCode != 200 {
		return "", responseError(r.StatusCode, b)
	}
	var v struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	if v.Token == "" {
		return "", errors.New("Devin returned no login token")
	}
	return NormalizeToken(v.Token), nil
}
