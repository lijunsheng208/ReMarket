package handler

import (
	"context"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
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

type currentReader struct {
	user  *model.User
	err   error
	id    int64
	calls int
}

// GetByID 捕获数据库查询身份，验证不会采用客户端伪造的用户头。
func (r *currentReader) GetByID(ctx context.Context, id int64) (*model.User, error) {
	r.calls++
	r.id = id
	return r.user, r.err
}

// TestCurrentUserRPC 验证匿名拒绝、JWT 验签、身份防伪造及最新账号状态检查。
func TestCurrentUserRPC(t *testing.T) {
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "api", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.IssueAccessToken(42, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	reader := &currentReader{user: &model.User{ID: 42, Username: "alice", Role: "USER", Status: "ACTIVE", CreatedAt: time.Now().UTC()}}
	current := service.NewCurrentUser(reader)
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens, userv1.UserService_Register_FullMethodName, userv1.UserService_Login_FullMethodName))
	userv1.RegisterUserServiceServer(server, NewAuthenticated(service.New(&fakeCreator{}), nil, current))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///current", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := userv1.NewUserServiceClient(conn)
	for _, md := range []metadata.MD{
		metadata.Pairs("x-user-id", "42", "x-role", "ADMIN"),
		metadata.Pairs("authorization", "Bearer invalid"),
		metadata.Pairs("authorization", "Basic "+raw),
		metadata.Pairs("authorization", "Bearer "+raw, "authorization", "Bearer "+raw),
	} {
		_, err := client.GetCurrentUser(metadata.NewOutgoingContext(context.Background(), md), &userv1.GetCurrentUserRequest{})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("接受无效凭据: %v", err)
		}
	}
	if reader.calls != 0 {
		t.Fatal("鉴权失败仍查询数据库")
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+raw, "x-user-id", "999", "x-role", "ADMIN"))
	result, err := client.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if reader.id != 42 || result.User.Id != 42 || result.User.Role != "USER" {
		t.Fatal("采用了伪造身份或过期角色")
	}
	reader.user.Status = "DISABLED"
	_, err = client.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal("禁用账号未拒绝")
	}
	reader.err = repository.ErrUserNotFound
	_, err = client.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("删除账号未拒绝")
	}
	reader.err = errors.New("database secret")
	_, err = client.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatal("内部错误未映射")
	}
	_, err = client.Register(context.Background(), &userv1.RegisterRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal("注册未匿名放行")
	}
	_, err = client.Login(context.Background(), &userv1.LoginRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatal("登录未匿名放行")
	}
	_, err = client.GetUser(context.Background(), &userv1.GetUserRequest{Id: 42})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("其他接口未默认保护")
	}
	_, err = current.GetCurrentUser(context.Background())
	if status.Code(grpcx.Error(err)) != codes.Unauthenticated {
		t.Fatal("直接业务调用未校验身份")
	}
}
