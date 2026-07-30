// Package cos implements a minimal Tencent Cloud Object Storage (COS) client
// using plain net/http plus the COS q-sign-algorithm=sha1 request signature
// (HMAC-SHA1). It intentionally avoids the official tenc-cos-go-sdk to keep the
// dependency surface stdlib-only at this milestone, mirroring pkg/wxsdk: the two
// operations the product needs today are PutObject (boss image upload) and
// SignedURL (temporary download link). The signing helpers are pure and take
// now/ttl as parameters so tests can fix time and verify a self-consistent
// signature.
//
// 签名规则参考腾讯云 COS「请求签名」文档（q-sign-algorithm=sha1）：
// https://cloud.tencent.com/document/product/436/7778
package cos

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultSignWindow 是单次签名默认的有效时长起点偏移：把 start 往前拨一点，
// 容忍客户端/服务端时钟轻微偏差（COS 也要求签名的 q-sign-time 覆盖请求时刻）。
const defaultSignWindow = 10 * time.Minute

// keyTime 返回 COS 签名的时间窗字符串 "<start>;<end>"（unix 秒）。
// 起点往前回退一个 defaultSignWindow 以容忍时钟漂移。
func keyTime(now time.Time, ttl time.Duration) string {
	start := now.Add(-defaultSignWindow).Unix()
	end := now.Add(ttl).Unix()
	return strconv.FormatInt(start, 10) + ";" + strconv.FormatInt(end, 10)
}

// hmacSHA1Hex 计算 HMAC-SHA1 并返回小写 hex 串。
func hmacSHA1Hex(key, data string) string {
	m := hmac.New(sha1.New, []byte(key))
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

// sha1Hex 计算 SHA1 并返回小写 hex 串。
func sha1Hex(data string) string {
	s := sha1.Sum([]byte(data)) //nolint:gosec // COS 规范要求 SHA1，非安全用途
	return hex.EncodeToString(s[:])
}

// signKey 计算 SignKey = HMAC-SHA1-hex(SecretKey, KeyTime)。
func signKey(secretKey, keyTime string) string {
	return hmacSHA1Hex(secretKey, keyTime)
}

// canonicalKV 把 map 规整为 COS 要求的串：key 转小写并 urlencode，value
// urlencode，按编码后 key 升序，以 "&" 连接。list 返回参与签名的 key 列表
// （小写、";" 连接）。COS 规定 key 先小写再 urlencode，排序也按编码后结果。
func canonicalKV(m map[string]string) (paramString, list string) {
	if len(m) == 0 {
		return "", ""
	}
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		lk := strings.ToLower(k)
		pairs = append(pairs, kv{k: url.QueryEscape(lk), v: url.QueryEscape(v)})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })

	strs := make([]string, 0, len(pairs))
	keys := make([]string, 0, len(pairs))
	for _, p := range pairs {
		strs = append(strs, p.k+"="+p.v)
		keys = append(keys, p.k)
	}
	return strings.Join(strs, "&"), strings.Join(keys, ";")
}

// buildAuthorization 组装 header 形式的 Authorization：
// q-sign-algorithm=sha1&q-ak=<SecretID>&q-sign-time=<kt>&q-key-time=<kt>&
// q-header-list=<...>&q-url-param-list=<...>&q-signature=<sig>。
//
// HttpString    = lower(method) + "\n" + urlPath + "\n" + paramString + "\n" + headerString + "\n"
// StringToSign  = "sha1\n" + keyTime + "\n" + sha1hex(HttpString) + "\n"
// Signature     = HMAC-SHA1-hex(SignKey, StringToSign)
func buildAuthorization(method, urlPath string, params, headers map[string]string, secretID, secretKey, keyTime string) string {
	sig, headerList, paramList := signature(method, urlPath, params, headers, secretKey, keyTime)
	return fmt.Sprintf(
		"q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%s&q-key-time=%s&q-header-list=%s&q-url-param-list=%s&q-signature=%s",
		url.QueryEscape(secretID), keyTime, keyTime, headerList, paramList, sig,
	)
}

// signature 计算 q-signature 及参与签名的 header/param key 列表。
// 供 header 与临时 URL 两种方案共用。
func signature(method, urlPath string, params, headers map[string]string, secretKey, keyTime string) (sig, headerList, paramList string) {
	paramString, pList := canonicalKV(params)
	headerString, hList := canonicalKV(headers)

	httpString := strings.ToLower(method) + "\n" + urlPath + "\n" + paramString + "\n" + headerString + "\n"
	stringToSign := "sha1\n" + keyTime + "\n" + sha1Hex(httpString) + "\n"
	sk := signKey(secretKey, keyTime)
	return hmacSHA1Hex(sk, stringToSign), hList, pList
}

// signedURL 生成临时下载 URL（临时密钥/匿名访问场景，签名放 query）。
// 与 header 方案用同一套 q-sign 计算，但把 q-* 参数拼到 query 上返回完整 URL。
func signedURL(method, baseURL, key string, params map[string]string, secretID, secretKey string, ttl time.Duration, now time.Time) string {
	kt := keyTime(now, ttl)
	// 临时 URL 方案的签名 header/param 为空集（下载 GET 通常无需参与签名的 header）。
	sig, _, _ := signature(method, objectPath(key), params, nil, secretKey, kt)

	u := strings.TrimRight(baseURL, "/") + objectPath(key)
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	q.Set("q-sign-algorithm", "sha1")
	q.Set("q-ak", secretID)
	q.Set("q-sign-time", kt)
	q.Set("q-key-time", kt)
	q.Set("q-header-list", "")
	q.Set("q-url-param-list", "")
	q.Set("q-signature", sig)
	return u + "?" + q.Encode()
}

// objectPath 返回对象的 URL path（保证 leading slash）。
func objectPath(key string) string {
	if strings.HasPrefix(key, "/") {
		return key
	}
	return "/" + key
}

// headerMap 从 http.Header 抽取参与签名的 headers 为小写 key map。
// 只取指定的 keys（如 host/content-type），避免把 SDK 自动加的 header 卷进签名。
func headerMap(h http.Header, keys ...string) map[string]string {
	m := make(map[string]string, len(keys))
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			m[strings.ToLower(k)] = v
		}
	}
	return m
}
