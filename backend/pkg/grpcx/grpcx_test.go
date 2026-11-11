package grpcx

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
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/apperror"
	"testing"
	"time"
)

type testUser struct {
	userv1.UnimplementedUserServiceServer
	observed chan context.Context
}

// GetUser 模拟不存在、等待超时及 panic 三种服务端行为。
func (s *testUser) GetUser(ctx context.Context, req *userv1.GetUserRequest) (*userv1.UserResponse, error) {
	s.observed <- ctx
	if req.Id == 1 {
		return nil, &apperror.Error{Kind: apperror.NotFound, Message: "user not found"}
	}
	if req.Id == 2 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	panic("sensitive panic")
}

// TestTransport 通过内存连接验证真实 gRPC 调用的元数据、超时与错误处理。
func TestTransport(t *testing.T) {
	// 使用内存监听器运行真实协议，避免测试依赖本机端口和外部服务。
	listener := bufconn.Listen(1 << 20)
	server := NewServer(nil)
	service := &testUser{observed: make(chan context.Context, 4)}
	userv1.RegisterUserServiceServer(server, service)
	productv1.RegisterProductServiceServer(server, &productv1.UnimplementedProductServiceServer{})
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithChainUnaryInterceptor(ClientInterceptor(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := WithRequestID(context.Background(), "request-123")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer test", "traceparent", "test-trace", "x-user-id", "999"))
	client := userv1.NewUserServiceClient(conn)
	var header metadata.MD
	_, err = client.GetUser(ctx, &userv1.GetUserRequest{Id: 1}, grpc.Header(&header))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("error mapping: %v", err)
	}
	observed := <-service.observed
	if RequestID(observed) != "request-123" {
		t.Fatal("request ID not propagated")
	}
	if len(header.Get("x-request-id")) != 1 || header.Get("x-request-id")[0] != "request-123" {
		t.Fatal("response request ID missing")
	}
	md, _ := metadata.FromIncomingContext(observed)
	if md.Get("authorization")[0] != "Bearer test" || md.Get("traceparent")[0] != "test-trace" {
		t.Fatal("metadata not propagated")
	}
	if len(md.Get("x-user-id")) != 0 {
		t.Fatal("untrusted identity propagated")
	}
	if _, ok := observed.Deadline(); !ok {
		t.Fatal("deadline missing")
	}
	// 用更短的上游期限确认拦截器不会延长 Deadline。
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = client.GetUser(short, &userv1.GetUserRequest{Id: 2})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("deadline: %v", err)
	}
	<-service.observed
	_, err = client.GetUser(ctx, &userv1.GetUserRequest{Id: 3})
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal server error" {
		t.Fatalf("panic recovery: %v", err)
	}
	<-service.observed
	_, err = productv1.NewProductServiceClient(conn).GetProduct(ctx, &productv1.GetProductRequest{Id: 1})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("product registration: %v", err)
	}
}

// TestErrors 验证业务错误及 Context 错误映射为预期 gRPC 状态码。
func TestErrors(t *testing.T) {
	cases := []struct {
		err  error
		code codes.Code
	}{
		{context.Canceled, codes.Canceled}, {context.DeadlineExceeded, codes.DeadlineExceeded},
		{errors.New("password=secret"), codes.Internal},
		{&apperror.Error{Kind: apperror.Invalid, Message: "invalid"}, codes.InvalidArgument},
		{&apperror.Error{Kind: apperror.Conflict, Message: "duplicate"}, codes.AlreadyExists},
		{&apperror.Error{Kind: apperror.Unauthenticated, Message: "login"}, codes.Unauthenticated},
		{&apperror.Error{Kind: apperror.Forbidden, Message: "forbidden"}, codes.PermissionDenied},
		{&apperror.Error{Kind: apperror.FailedPrecondition, Message: "state"}, codes.FailedPrecondition},
		{&apperror.Error{Kind: apperror.Unavailable, Message: "unavailable"}, codes.Unavailable},
	}
	for _, c := range cases {
		if status.Code(Error(c.err)) != c.code {
			t.Errorf("%v", c.err)
		}
	}
	if Error(nil) != nil {
		t.Fatal("nil error changed")
	}
}
