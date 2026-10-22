package devin

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

const MaxClientIdentitySize = 32 << 10

//go:embed client_identity.default.json
var defaultClientIdentityJSON []byte

// ClientInfo contains declarations only. Credentials, API hosts and protocol
// capabilities cannot be supplied by a remotely published identity document.
type ClientInfo struct {
	IDEName          string `json:"ide_name"`
	IDEType          string `json:"ide_type,omitempty"`
	IDEVersion       string `json:"ide_version"`
	ExtensionName    string `json:"extension_name"`
	ExtensionVersion string `json:"extension_version"`
	OS               string `json:"os"`
	Locale           string `json:"locale"`
}

type ClientIdentity struct {
	SchemaVersion int        `json:"schema_version"`
	Revision      string     `json:"revision"`
	Chat          ClientInfo `json:"chat"`
	Discovery     ClientInfo `json:"discovery"`
	TokenExchange ClientInfo `json:"token_exchange"`
}

var identityValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var identityVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)

func (v ClientIdentity) Validate() error {
	if v.SchemaVersion != 1 {
		return errors.New("unsupported Devin client identity schema_version (expected 1)")
	}
	if !identityValuePattern.MatchString(v.Revision) {
		return errors.New("invalid Devin client identity revision")
	}
	for name, info := range map[string]ClientInfo{"chat": v.Chat, "discovery": v.Discovery, "token_exchange": v.TokenExchange} {
		for field, value := range map[string]string{
			"ide_name": info.IDEName, "ide_version": info.IDEVersion,
			"extension_name": info.ExtensionName, "extension_version": info.ExtensionVersion,
			"os": info.OS, "locale": info.Locale,
		} {
			if !identityValuePattern.MatchString(value) {
				return fmt.Errorf("invalid Devin client identity %s.%s", name, field)
			}
		}
		if info.IDEType != "" && !identityValuePattern.MatchString(info.IDEType) {
			return fmt.Errorf("invalid Devin client identity %s.ide_type", name)
		}
		if !identityVersionPattern.MatchString(info.IDEVersion) || !identityVersionPattern.MatchString(info.ExtensionVersion) {
			return fmt.Errorf("invalid Devin client identity %s version (expected x.y.z or x.y.z-suffix)", name)
		}
	}
	return nil
}

func ParseClientIdentity(data []byte) (ClientIdentity, error) {
	var v ClientIdentity
	if len(data) > MaxClientIdentitySize {
		return v, errors.New("devin client identity document exceeds 32 KB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, fmt.Errorf("invalid Devin client identity JSON: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return v, errors.New("devin client identity must contain one JSON object")
	}
	return v, v.Validate()
}

var builtinClientIdentity = func() ClientIdentity {
	v, err := ParseClientIdentity(defaultClientIdentityJSON)
	if err != nil {
		panic(err)
	}
	return v
}()

func DefaultClientIdentity() ClientIdentity { return builtinClientIdentity }

var clientIdentityResolver atomic.Pointer[func(context.Context) ClientIdentity]

func SetClientIdentityResolver(resolver func(context.Context) ClientIdentity) {
	if resolver == nil {
		clientIdentityResolver.Store(nil)
		return
	}
	clientIdentityResolver.Store(&resolver)
}

type clientIdentityContextKey struct{}

// WithClientIdentity pins all clients and runtime cache lookups for one incoming
// request, including detached singleflight work, to the same profile revision.
func WithClientIdentity(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(clientIdentityContextKey{}).(ClientIdentity); ok {
		return ctx
	}
	return context.WithValue(ctx, clientIdentityContextKey{}, ResolveClientIdentity(ctx))
}

func ResolveClientIdentity(ctx context.Context) ClientIdentity {
	if ctx != nil {
		if v, ok := ctx.Value(clientIdentityContextKey{}).(ClientIdentity); ok {
			return v
		}
	}
	if resolver := clientIdentityResolver.Load(); resolver != nil {
		if v := (*resolver)(ctx); v.Validate() == nil {
			return v
		}
	}
	return DefaultClientIdentity()
}

func (v ClientIdentity) Fingerprint() string {
	b, _ := json.Marshal(v)
	return CredentialFingerprint(string(b))
}

func (v ClientInfo) metadata(token string) *pb.Metadata {
	return &pb.Metadata{
		ApiKey: NormalizeToken(token), IdeName: v.IDEName, IdeType: v.IDEType,
		IdeVersion: v.IDEVersion, ExtensionName: v.ExtensionName, ExtensionVersion: v.ExtensionVersion,
		Os: v.OS, Locale: v.Locale, DisableTelemetry: true,
	}
}

func (c *Client) identity() ClientIdentity {
	if c.Identity != nil {
		return *c.Identity
	}
	return DefaultClientIdentity()
}
