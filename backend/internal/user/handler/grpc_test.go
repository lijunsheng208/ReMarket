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
	"testing"
	"time"
)

type fakeCreator struct {
	input repository.CreateUserParams
	calls int
	err   error
}

// Create 捕获真正传入 Repository 的参数，模拟唯一冲突及内部错误。
func (f *fakeCreator) Create(ctx context.Context, p repository.CreateUserParams) (*model.User, error) {
	f.calls++
	f.input = p
	if f.err != nil {
		return nil, f.err
	}
	return &model.User{ID: 1, Username: p.Username, Nickname: p.Nickname, PasswordHash: p.PasswordHash, Role: model.RoleUser, Status: model.StatusActive, CreatedAt: time.Now().UTC()}, nil
}

// TestRegisterRPC 验证协议适配、用户名规范化、密码哈希及统一错误映射。
func TestRegisterRPC(t *testing.T) {
	repo := &fakeCreator{}
	server := grpcx.NewServer(nil)
	userv1.RegisterUserServiceServer(server, New(service.New(repo)))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///register", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := userv1.NewUserServiceClient(conn)
	request := &userv1.RegisterRequest{Username: " Alice_1 ", Nickname: " 测试用户 ", Password: " password123 "}
	result, err := client.Register(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Username != "alice_1" || result.User.Nickname != "测试用户" || result.User.Id != 1 {
		t.Fatal("响应或规范化不正确")
	}
	if repo.input.PasswordHash == request.Password {
		t.Fatal("明文入库")
	}
	if err := auth.VerifyPassword(repo.input.PasswordHash, request.Password); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*userv1.RegisterRequest{
		{Username: "ab", Nickname: "用户", Password: "password123"},
		{Username: "中文user", Nickname: "用户", Password: "password123"},
		{Username: "valid_user", Nickname: "  ", Password: "password123"},
		{Username: "valid_user", Nickname: "用户", Password: "short"},
	} {
		before := repo.calls
		_, err := client.Register(context.Background(), r)
		if status.Code(err) != codes.InvalidArgument || repo.calls != before {
			t.Fatalf("校验失败或仍然入库: %v", err)
		}
	}
	repo.err = repository.ErrUsernameExists
	_, err = client.Register(context.Background(), request)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("冲突映射: %v", err)
	}
	repo.err = errors.New("database secret")
	_, err = client.Register(context.Background(), request)
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal server error" {
		t.Fatalf("内部错误泄漏: %v", err)
	}
	_, err = client.Login(context.Background(), &userv1.LoginRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("登录应保持未实现: %v", err)
	}
}

// TestRegisterCanceled 验证已取消请求及空请求不会触发数据库写入。
func TestRegisterCanceled(t *testing.T) {
	repo := &fakeCreator{}
	svc := service.New(repo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Register(ctx, service.RegisterInput{Username: "alice", Nickname: "用户", Password: "password123"})
	if !errors.Is(err, context.Canceled) || repo.calls != 0 {
		t.Fatal("取消请求仍然处理")
	}
	_, err = New(svc).Register(context.Background(), nil)
	if status.Code(grpcx.Error(err)) != codes.InvalidArgument {
		t.Fatal("空请求未拒绝")
	}
}
