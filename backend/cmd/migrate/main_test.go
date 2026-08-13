package main

// main_test.go 锁定 seed-account 的参数解析红线: 只认 --reset-password,
// 任何其它参数(尤其是任何形式的密码参数)一律拒绝——密码绝不许出现在
// CLI 参数表(ps / shell history 会留痕)。

import "testing"

func TestParseSeedAccountArgs(t *testing.T) {
	// 无参数: 不重置。
	reset, err := parseSeedAccountArgs(nil)
	if err != nil || reset {
		t.Fatalf("parseSeedAccountArgs(nil) = (%v, %v), want (false, nil)", reset, err)
	}

	// 显式重置标志。
	reset, err = parseSeedAccountArgs([]string{"--reset-password"})
	if err != nil || !reset {
		t.Fatalf("parseSeedAccountArgs(--reset-password) = (%v, %v), want (true, nil)", reset, err)
	}

	// 密码类/未知参数一律拒绝(密码只走 env ZG_BOOTSTRAP_BOSS_PASSWORD)。
	for _, args := range [][]string{
		{"--password", "x"},
		{"--boss-password=y"},
		{"somepassword"},
		{"-p"},
	} {
		if _, err := parseSeedAccountArgs(args); err == nil {
			t.Errorf("parseSeedAccountArgs(%v) = nil error, want rejection", args)
		}
	}
}
