package handler

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
)

// TestProductServiceAuth 通过真实 gRPC 验证四个内部方法、用户方法及禁用配置的凭据隔离。
func TestProductServiceAuth(t *testing.T) {
	secret := strings.Repeat("j", 32)
	serviceToken := strings.Repeat("s", 32)
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: secret, Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := tokens.IssueAccessToken(1, "USER")
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := tokens.IssueAccessToken(2, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{serviceToken, ""} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[configured != ""], func(t *testing.T) {
			interceptor, err := NewAuthInterceptor(tokens, configured, secret)
			if err != nil {
				t.Fatal(err)
			}
			listener := bufconn.Listen(1 << 20)
			server := grpcx.NewServer(nil, interceptor)
			productv1.RegisterProductServiceServer(server, &productv1.UnimplementedProductServiceServer{})
			go server.Serve(listener)
			defer server.Stop()
			defer listener.Close()
			conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			methods := []string{productv1.ProductService_ReserveProduct_FullMethodName, productv1.ProductService_GetReservation_FullMethodName, productv1.ProductService_ReleaseReservation_FullMethodName, productv1.ProductService_ConfirmReservation_FullMethodName}
			for _, method := range methods {
				cases := []struct {
					name string
					md   metadata.MD
					want codes.Code
				}{
					{"missing", nil, codes.Unauthenticated},
					{"user", metadata.Pairs("authorization", "Bearer "+user), codes.Unauthenticated},
					{"admin", metadata.Pairs("authorization", "Bearer "+admin), codes.Unauthenticated},
					{"forged", metadata.Pairs("x-user-id", "1", "x-role", "ORDER"), codes.Unauthenticated},
					{"wrong", metadata.Pairs("authorization", "Bearer wrong"), codes.Unauthenticated},
					{"duplicate", metadata.Pairs("authorization", "Bearer "+serviceToken, "authorization", "Bearer "+serviceToken), codes.Unauthenticated},
					{"service", metadata.Pairs("authorization", "Bearer "+serviceToken), codes.Unimplemented},
				}
				for _, tc := range cases {
					ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), tc.md), time.Second)
					err := conn.Invoke(ctx, method, &emptypb.Empty{}, &emptypb.Empty{})
					cancel()
					want := tc.want
					if configured == "" {
						want = codes.Unauthenticated
					}
					if status.Code(err) != want {
						t.Errorf("%s %s: got %v want %v", method, tc.name, status.Code(err), want)
					}
				}
			}
			// 服务凭据不能用于普通用户写接口，用户 JWT 的原鉴权语义保持有效。
			for _, tc := range []struct {
				token string
				want  codes.Code
			}{{serviceToken, codes.Unauthenticated}, {user, codes.Unimplemented}} {
				ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tc.token)), time.Second)
				err := conn.Invoke(ctx, productv1.ProductService_CreateProduct_FullMethodName, &emptypb.Empty{}, &emptypb.Empty{})
				cancel()
				if status.Code(err) != tc.want {
					t.Errorf("用户接口凭据隔离失败：%v", status.Code(err))
				}
			}
		})
	}
}

// TestProductServiceAuthConfig 验证弱配置、空白及密钥复用被拒绝且错误不泄露密钥。
func TestProductServiceAuthConfig(t *testing.T) {
	secret := strings.Repeat("j", 32)
	for _, token := range []string{"short", secret, strings.Repeat("s", 32) + "\n"} {
		_, err := NewAuthInterceptor(nil, token, secret)
		if err == nil {
			t.Fatal("非法服务凭据配置被接受")
		}
		if strings.Contains(err.Error(), token) {
			t.Fatal("错误包含服务凭据")
		}
	}
}
