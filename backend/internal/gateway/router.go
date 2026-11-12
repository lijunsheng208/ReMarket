// Package gateway 将 HTTP 请求适配为业务服务的 gRPC 调用。
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"mime"
	"net/http"
	"regexp"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/grpcx"
	"time"
)

type UserClient interface {
	Register(context.Context, *userv1.RegisterRequest, ...grpc.CallOption) (*userv1.UserResponse, error)
	Login(context.Context, *userv1.LoginRequest, ...grpc.CallOption) (*userv1.LoginResponse, error)
}
type Handler struct {
	users   UserClient
	timeout time.Duration
}

var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// NewRouter 创建匿名注册和登录路由，不记录请求体、密码或响应 Token。
func NewRouter(users UserClient, timeout time.Duration) *gin.Engine {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	h := &Handler{users: users, timeout: timeout}
	router := gin.New()
	_ = router.SetTrustedProxies(nil)
	router.Use(requestContext(), recoverRequest())
	router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	router.POST("/api/v1/auth/register", h.register)
	router.POST("/api/v1/auth/login", h.login)
	router.NoRoute(func(c *gin.Context) { writeError(c, http.StatusNotFound, "NOT_FOUND", "接口不存在") })
	return router
}

// requestContext 校验或生成请求编号，并从原 HTTP Context 派生供下游调用使用。
func requestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				writeError(c, 500, "INTERNAL", "服务内部错误")
				c.Abort()
				return
			}
			id = hex.EncodeToString(bytes[:])
		}
		c.Header("X-Request-ID", id)
		ctx := grpcx.WithRequestID(c.Request.Context(), id)
		// 只传递追踪字段；匿名接口不转发用户伪造的身份头及认证信息。
		md := metadata.MD{}
		for _, key := range []string{"traceparent", "tracestate"} {
			if value := c.GetHeader(key); value != "" {
				md.Set(key, value)
			}
		}
		c.Request = c.Request.WithContext(metadata.NewOutgoingContext(ctx, md))
		c.Next()
	}
}

// recoverRequest 捕获当前 HTTP goroutine 的 panic，不将 panic 值或敏感请求头输出日志。
func recoverRequest() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				c.Abort()
				writeError(c, 500, "INTERNAL", "服务内部错误")
			}
		}()
		c.Next()
	}
}

// decodeJSON 限制请求体为 16 KiB，拒绝未知字段、非 JSON 及多段 JSON。
func decodeJSON(c *gin.Context, target any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(c, 415, "UNSUPPORTED_MEDIA_TYPE", "请使用 application/json")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(target)
	if err == nil {
		var extra any
		err = decoder.Decode(&extra)
		if err == io.EOF {
			return true
		}
		if err == nil {
			err = errors.New("multiple values")
		}
	}
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		writeError(c, 413, "PAYLOAD_TOO_LARGE", "请求体过大")
	} else {
		writeError(c, 400, "INVALID_ARGUMENT", "请求 JSON 无效")
	}
	return false
}

// register 只做协议适配，用户名及密码业务校验由 User Service 负责。
func (h *Handler) register(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	if !decodeJSON(c, &input) {
		return
	}
	// 沿用 HTTP 取消信号，WithTimeout 不会延长上游更早的 Deadline。
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.users.Register(ctx, &userv1.RegisterRequest{Username: input.Username, Password: input.Password, Nickname: input.Nickname})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	writeProto(c, http.StatusCreated, result)
}

// login 转发登录凭据，成功响应保留 Token 和过期时间，禁止中间缓存。
func (h *Handler) login(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(c, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.users.Login(ctx, &userv1.LoginRequest{Username: input.Username, Password: input.Password})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	writeProto(c, http.StatusOK, result)
}

// writeProto 使用 Proto JSON 规则输出 snake_case 字段，int64 ID 为字符串。
func writeProto(c *gin.Context, httpStatus int, message proto.Message) {
	data, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(message)
	if err != nil {
		writeError(c, 500, "INTERNAL", "响应序列化失败")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(httpStatus, "application/json; charset=utf-8", data)
}

// writeError 统一错误结构，附带请求编号供调用方定位问题。
func writeError(c *gin.Context, httpStatus int, code, message string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(httpStatus, gin.H{"error": gin.H{"code": code, "message": message, "request_id": grpcx.RequestID(c.Request.Context())}})
}

// writeRPCError 将 gRPC 失败转换为 HTTP，内部故障不透传下游说明。
func writeRPCError(c *gin.Context, err error) {
	st := status.Convert(grpcx.Error(err))
	httpStatus := 500
	message := st.Message()
	switch st.Code() {
	case codes.InvalidArgument:
		httpStatus = 400
	case codes.Unauthenticated:
		httpStatus = 401
	case codes.PermissionDenied:
		httpStatus = 403
	case codes.NotFound:
		httpStatus = 404
	case codes.AlreadyExists, codes.Aborted, codes.FailedPrecondition:
		httpStatus = 409
	case codes.ResourceExhausted:
		httpStatus = 429
		message = "请求过于频繁"
	case codes.DeadlineExceeded:
		httpStatus = 504
		message = "上游服务响应超时"
	case codes.Unavailable:
		httpStatus = 503
		message = "上游服务暂不可用"
	case codes.Canceled:
		httpStatus = 499
		message = "请求已取消"
	case codes.Unimplemented:
		httpStatus = 501
		message = "接口尚未实现"
	default:
		message = "服务内部错误"
	}
	writeError(c, httpStatus, st.Code().String(), message)
}
