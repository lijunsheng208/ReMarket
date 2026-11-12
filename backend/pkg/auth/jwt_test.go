package auth

import (
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"strings"
	"testing"
	"time"
)

// testJWTManager 创建固定时钟组件，避免通过 sleep 验证时间边界。
func testJWTManager(t *testing.T) *JWTManager {
	t.Helper()
	m, err := NewJWTManager(JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "remarket-api", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Unix(1800000000, 0).UTC() }
	return m
}

// TestJWTLifecycle 验证签发内容和到期边界，过期时刻本身也应拒绝。
func TestJWTLifecycle(t *testing.T) {
	m := testJWTManager(t)
	raw, expires, err := m.IssueAccessToken(123, "USER")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := m.VerifyAccessToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "123" || claims.Role != "USER" || !expires.Equal(m.now().Add(time.Hour)) {
		t.Fatal("声明或过期时间不正确")
	}
	m.now = func() time.Time { return expires.Add(-time.Second) }
	if _, err := m.VerifyAccessToken(raw); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return expires }
	if _, err := m.VerifyAccessToken(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("接受过期令牌")
	}
}

// TestJWTRejectsInvalidTokens 验证签名、算法、必需声明与身份校验不会被绕过。
func TestJWTRejectsInvalidTokens(t *testing.T) {
	m := testJWTManager(t)
	raw, _, err := m.IssueAccessToken(123, "USER")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(raw, ".")
	parts[1] = "eyJzdWIiOiI5OTkifQ"
	for _, raw := range []string{"", "malformed", strings.Join(parts, "."), strings.Repeat("x", 8193)} {
		if _, err := m.VerifyAccessToken(raw); !errors.Is(err, ErrInvalidToken) {
			t.Fatal("接受无效令牌")
		}
	}
	tests := []string{"wrong-key", "HS384", "none", "issuer", "audience", "missing-exp", "missing-iat", "missing-nbf", "future-iat", "future-nbf", "bad-subject", "bad-role"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			now := m.now()
			claims := AccessClaims{Role: "USER", RegisteredClaims: jwt.RegisteredClaims{Issuer: m.issuer, Audience: jwt.ClaimStrings{m.audience}, Subject: "123", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}}
			method := jwt.SigningMethodHS256
			var key any = m.secret
			switch name {
			case "wrong-key":
				key = []byte(strings.Repeat("x", 32))
			case "HS384":
				method = jwt.SigningMethodHS384
			case "issuer":
				claims.Issuer = "other"
			case "audience":
				claims.Audience = jwt.ClaimStrings{"other"}
			case "missing-exp":
				claims.ExpiresAt = nil
			case "missing-iat":
				claims.IssuedAt = nil
			case "missing-nbf":
				claims.NotBefore = nil
			case "future-iat":
				claims.IssuedAt = jwt.NewNumericDate(now.Add(time.Minute))
			case "future-nbf":
				claims.NotBefore = jwt.NewNumericDate(now.Add(time.Minute))
			case "bad-subject":
				claims.Subject = "0"
			case "bad-role":
				claims.Role = "SUPERADMIN"
			}
			token := jwt.NewWithClaims(method, claims)
			if name == "none" {
				token = jwt.NewWithClaims(jwt.SigningMethodNone, claims)
				key = jwt.UnsafeAllowNoneSignatureType
			}
			signed, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.VerifyAccessToken(signed); !errors.Is(err, ErrInvalidToken) {
				t.Fatal("接受不可信令牌")
			}
		})
	}
}

// TestJWTConfiguration 验证密钥、有效期与环境变量配置在启动前明确失败。
func TestJWTConfiguration(t *testing.T) {
	valid := JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "remarket-api", AccessTTL: time.Hour}
	for _, name := range []string{"secret", "issuer", "audience", "zero-ttl", "long-ttl"} {
		c := valid
		switch name {
		case "secret":
			c.Secret = "short"
		case "issuer":
			c.Issuer = ""
		case "audience":
			c.Audience = ""
		case "zero-ttl":
			c.AccessTTL = 0
		case "long-ttl":
			c.AccessTTL = 25 * time.Hour
		}
		if _, err := NewJWTManager(c); !errors.Is(err, ErrJWTConfig) {
			t.Fatal("接受无效配置")
		}
	}
	m := testJWTManager(t)
	if _, _, err := m.IssueAccessToken(0, "USER"); !errors.Is(err, ErrInvalidTokenIdentity) {
		t.Fatal("接受零 ID")
	}
	if _, _, err := m.IssueAccessToken(1, "ROOT"); !errors.Is(err, ErrInvalidTokenIdentity) {
		t.Fatal("接受未知角色")
	}
	t.Setenv("JWT_SECRET", valid.Secret)
	t.Setenv("JWT_ISSUER", valid.Issuer)
	t.Setenv("JWT_AUDIENCE", valid.Audience)
	t.Setenv("JWT_ACCESS_TTL", "1h")
	if _, err := JWTConfigFromEnv(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_ACCESS_TTL", "invalid")
	if _, err := JWTConfigFromEnv(); !errors.Is(err, ErrJWTConfig) {
		t.Fatal("接受错误时长")
	}
}
