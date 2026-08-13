package auth

// password_strength_test.go 锁定引导密码强度策略: 弱密码拒绝 / 强密码放行。
// 这是 seed-account 密码安全验收的第一道闸(纯函数, 不触 DB)。

import "testing"

func TestValidatePasswordStrength_Rejects(t *testing.T) {
	weak := []string{
		"",              // 空(env 未设置)
		"short1a",       // 太短(<10)
		"12345678901",   // 纯数字
		"abcdefghijklm", // 纯字母
		"password123",   // 弱密码块表
		"Password123",   // 块表大小写不敏感
		"1234567890",    // 块表(长度达标仍拦)
		"boss12345",     // dev 种子密码在块表(生产引导禁用)
		"ops12345",
	}
	for _, pw := range weak {
		if err := ValidatePasswordStrength(pw); err == nil {
			t.Errorf("ValidatePasswordStrength(%q) = nil, want error", pw)
		}
	}
}

func TestValidatePasswordStrength_Accepts(t *testing.T) {
	strong := []string{
		"Xk9mQ2vL8pz0",    // 字母+数字 ≥10
		"correcthorse9",   // 全小写字母+数字
		" openssl-rand-1", // 含符号不影响字母+数字判定
	}
	for _, pw := range strong {
		if err := ValidatePasswordStrength(pw); err != nil {
			t.Errorf("ValidatePasswordStrength(%q) = %v, want nil", pw, err)
		}
	}
}
