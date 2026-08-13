package auth

// password_strength.go — 冷启动引导密码强度策略 (cmd/migrate seed-account)。
//
// 密码安全红线(本机制核心验收点): 明文密码只在 env → ValidatePasswordStrength
// → HashPassword 之间短暂停留; 本文件返回的错误只描述规则, 绝不回显密码本身
// (明文绝不进日志 / CLI 参数表 / git)。

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// minBootstrapPasswordLen 引导密码最小长度(按符文计)。
const minBootstrapPasswordLen = 10

// weakBootstrapPasswords 常见弱密码小块表(全小写比对)。短于 10 位的弱密码
// 已被长度规则拦截, 这里同时列出其 ≥10 位变体兜底。dev 内存种子的
// "boss12345"/"ops12345" 也在表内——它们只存在于无 PG 的本地 dev 种子,
// 生产引导路径一律过本校验。
var weakBootstrapPasswords = map[string]struct{}{
	"password":    {},
	"password1":   {},
	"password123": {},
	"123456":      {},
	"1234567890":  {},
	"123456789a":  {},
	"qwerty12345": {},
	"admin12345":  {},
	"admin123456": {},
	"boss12345":   {},
	"boss123456":  {},
	"ops12345":    {},
	"1111111111":  {},
	"abc1234567":  {},
	"zhanggui123": {},
}

// ValidatePasswordStrength 强制引导密码策略: ≥10 位、同时含字母与数字、
// 不命中常见弱密码块表。返回的错误只描述规则, 绝不携带密码明文。
func ValidatePasswordStrength(pw string) error {
	if pw == "" {
		return errors.New("auth: 密码为空(需 export ZG_BOOTSTRAP_BOSS_PASSWORD)")
	}
	if utf8.RuneCountInString(pw) < minBootstrapPasswordLen {
		return errors.New("auth: 密码强度不足: 至少 10 位")
	}
	var hasLetter, hasDigit bool
	for _, r := range pw {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return errors.New("auth: 密码强度不足: 需同时包含字母和数字")
	}
	if _, weak := weakBootstrapPasswords[strings.ToLower(pw)]; weak {
		return errors.New("auth: 密码强度不足: 命中常见弱密码")
	}
	return nil
}
