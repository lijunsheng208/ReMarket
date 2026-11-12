package auth

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestPasswordRoundTrip 验证正确密码、随机盐、错误密码及默认计算成本。
func TestPasswordRoundTrip(t *testing.T) {
	password := "正确的密码示例123"
	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == password {
		t.Fatal("hash must use random salt and differ from plaintext")
	}
	if err := VerifyPassword(first, password); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(first, "incorrect-password"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("unexpected error: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(first))
	if err != nil || cost != bcrypt.DefaultCost {
		t.Fatalf("unexpected bcrypt cost: %d, %v", cost, err)
	}
}

// TestPasswordLimits 验证字符与字节边界，并确保生成和校验采用一致的输入限制。
func TestPasswordLimits(t *testing.T) {
	for _, password := range []string{"", "1234567", strings.Repeat("a", 73), strings.Repeat("中", 25), "12345678\xff"} {
		if _, err := HashPassword(password); !errors.Is(err, ErrInvalidPassword) {
			t.Errorf("invalid input accepted by hash: %v", err)
		}
		if err := VerifyPassword("", password); !errors.Is(err, ErrInvalidPassword) {
			t.Errorf("invalid input accepted by verify: %v", err)
		}
	}
	for _, password := range []string{"12345678", strings.Repeat("a", 72), strings.Repeat("中", 24), "  password  "} {
		hash, err := HashPassword(password)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyPassword(hash, password); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPasswordNoTruncation 验证不会截断超长输入，也不会自动去掉密码首尾空格。
func TestPasswordNoTruncation(t *testing.T) {
	password := strings.Repeat("a", 72)
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(hash, password+"x"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("long password accepted: %v", err)
	}
	hash, err = HashPassword(" password ")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(hash, "password"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("whitespace was normalized: %v", err)
	}
}

// TestInvalidStoredHash 验证空值、无效编码和不支持的版本不会被当成正确密码。
func TestInvalidStoredHash(t *testing.T) {
	for _, hash := range []string{"", "not-a-hash", strings.Repeat("x", 60), "$9a$10$" + strings.Repeat("x", 53)} {
		if err := VerifyPassword(hash, "password123"); !errors.Is(err, ErrInvalidPasswordHash) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}
