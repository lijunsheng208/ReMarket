package handler

import (
	"context"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"net"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/repository"
	"remarket/backend/internal/user/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
	"strings"
	"testing"
	"time"
)

type loginReader struct {
	user     *model.User
	err      error
	username string
}

// GetByUsername 模拟用户查询并记录规范化后的用户名。
func (r *loginReader) GetByUsername(ctx context.Context, name string) (*model.User, error) {
	r.username = name
	return r.user, r.err
}

// TestLoginRPC 使用真实业务、bcrypt 和 JWT 验证登录响应及统一失败提示。
func TestLoginRPC(t *testing.T) {
	hash, err := auth.HashPassword(" password123 ")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "remarket-api", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	user := &model.User{ID: 42, Username: "alice", PasswordHash: hash, Nickname: "用户", Role: "USER", Status: "ACTIVE", CreatedAt: time.Now().UTC()}
	reader := &loginReader{user: user}
	login, err := service.NewLogin(reader, tokens)
	if err != nil {
		t.Fatal(err)
	}
	server := grpcx.NewServer(nil)
	userv1.RegisterUserServiceServer(server, NewWithLogin(nil, login))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///login", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := userv1.NewUserServiceClient(conn)
	result, err := client.Login(context.Background(), &userv1.LoginRequest{Username: " ALICE ", Password: " password123 "})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := tokens.VerifyAccessToken(result.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if reader.username != "alice" || claims.Subject != "42" || claims.Role != "USER" || !claims.ExpiresAt.Time.Equal(result.ExpiresAt.AsTime()) || result.User.Id != 42 {
		t.Fatal("身份、规范化或过期时间错误")
	}
	for _, name := range []string{"missing", "wrong-password", "disabled", "short-password", "invalid-username"} {
		t.Run(name, func(t *testing.T) {
			reader.err = nil
			user.Status = "ACTIVE"
			req := &userv1.LoginRequest{Username: "alice", Password: " password123 "}
			switch name {
			case "missing":
				reader.err = repository.ErrUserNotFound
			case "wrong-password":
				req.Password = "wrong-password"
			case "disabled":
				user.Status = "DISABLED"
			case "short-password":
				req.Password = "x"
			case "invalid-username":
				req.Username = "!"
			}
			response, err := client.Login(context.Background(), req)
			if response != nil || status.Code(err) != codes.Unauthenticated || status.Convert(err).Message() != service.ErrLoginFailed.Message {
				t.Fatalf("失败提示不统一: %v", err)
			}
		})
	}
	reader.err = errors.New("database secret")
	user.Status = "ACTIVE"
	_, err = client.Login(context.Background(), &userv1.LoginRequest{Username: "alice", Password: " password123 "})
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal server error" {
		t.Fatal("数据库错误泄漏")
	}
	reader.err = nil
	user.PasswordHash = "corrupted"
	_, err = client.Login(context.Background(), &userv1.LoginRequest{Username: "alice", Password: " password123 "})
	if status.Code(err) != codes.Internal {
		t.Fatal("损坏哈希未作为内部故障处理")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := login.Login(ctx, service.LoginInput{}); !errors.Is(err, context.Canceled) {
		t.Fatal("取消未保留")
	}
	if _, err := NewWithLogin(nil, login).Login(context.Background(), nil); !errors.Is(err, service.ErrLoginFailed) {
		t.Fatal("空请求失败提示不一致")
	}
}
