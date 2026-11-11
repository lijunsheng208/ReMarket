// Package apperror 定义独立于传输协议的业务错误。
package apperror

type Kind int

const (
	Invalid Kind = iota + 1
	NotFound
	Conflict
	Unauthenticated
	Forbidden
	FailedPrecondition
	Unavailable
)

type Error struct {
	Kind    Kind
	Message string
}

// Error 返回可公开的业务错误说明，由传输层负责转换状态码。
func (e *Error) Error() string { return e.Message }
