package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Web Push（浏览器推送）：消息按 RFC 8291（aes128gcm）加密，用 VAPID（RFC 8292）
// 证明是本面板发的，直接 POST 到浏览器给的推送服务地址（Chrome 是 FCM，
// Firefox 是 Mozilla autopush，Edge 是 WNS）。全部用标准库实现。

const pushRecordSize = 4096

var b64u = base64.RawURLEncoding

// decodeB64 同时接受带 / 不带填充的 base64url 和标准 base64（浏览器给的是 base64url）。
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return b64u.DecodeString(s)
}

// encryptPush 按 RFC 8291 加密一条推送：uaPublic 是订阅里的 p256dh（65 字节未压缩点），
// authSecret 是订阅里的 auth（16 字节）。asPriv / salt 为 nil 时随机生成（测试时传固定值）。
func encryptPush(uaPublic, authSecret, plaintext []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("浏览器给的公钥不对：%v", err)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("浏览器给的 auth 不是 16 字节")
	}
	if asPriv == nil {
		if asPriv, err = curve.GenerateKey(rand.Reader); err != nil {
			return nil, err
		}
	}
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
	}
	asPublic := asPriv.PublicKey().Bytes()
	secret, err := asPriv.ECDH(ua)
	if err != nil {
		return nil, err
	}
	prkKey, err := hkdf.Extract(sha256.New, secret, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(plaintext)+1+gcm.Overhead() > pushRecordSize {
		return nil, errors.New("推送内容太长")
	}
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(pushRecordSize))
	out.WriteByte(byte(len(asPublic)))
	out.Write(asPublic)
	out.Write(gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil))
	return out.Bytes(), nil
}

// vapidKey 是面板的 VAPID 密钥对，第一次用时生成，存在 settings 的 "vapid"。
func (s *Server) vapidKey() (*ecdsa.PrivateKey, error) {
	s.vapidMu.Lock()
	defer s.vapidMu.Unlock()
	var stored string
	if _, err := s.st.Get("vapid", &stored); err != nil {
		return nil, err
	}
	if stored != "" {
		der, err := base64.StdEncoding.DecodeString(stored)
		if err != nil {
			return nil, err
		}
		k, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, err
		}
		if ek, ok := k.(*ecdsa.PrivateKey); ok {
			return ek, nil
		}
		return nil, errors.New("存的 VAPID 密钥不是 P-256")
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := s.st.Set("vapid", base64.StdEncoding.EncodeToString(der)); err != nil {
		return nil, err
	}
	return k, nil
}

func vapidPublic(k *ecdsa.PrivateKey) (string, error) {
	pub, err := k.PublicKey.ECDH()
	if err != nil {
		return "", err
	}
	return b64u.EncodeToString(pub.Bytes()), nil
}

// vapidJWT 签一个 ES256 的 JWT：aud 是推送服务的 origin，sub 是联系方式（网址或 mailto）。
func vapidJWT(k *ecdsa.PrivateKey, aud, sub string, exp time.Time) (string, error) {
	head := b64u.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "sub": sub})
	if err != nil {
		return "", err
	}
	signing := head + "." + b64u.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, sv, err := ecdsa.Sign(rand.Reader, k, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return signing + "." + b64u.EncodeToString(sig), nil
}

var errSubGone = errors.New("这个浏览器的订阅已失效")

var pushHTTP = &http.Client{Timeout: 20 * time.Second}

// sendPush 把 payload 推给一个订阅。返回 errSubGone 时应删掉这个订阅。
func (s *Server) sendPush(sub pushSub, payload []byte, contact string, urgency string) error {
	ep, err := url.Parse(sub.Endpoint)
	if err != nil || ep.Scheme != "https" || ep.Host == "" {
		return errors.New("推送地址不对")
	}
	uaPub, err := decodeB64(sub.P256dh)
	if err != nil {
		return err
	}
	auth, err := decodeB64(sub.Auth)
	if err != nil {
		return err
	}
	body, err := encryptPush(uaPub, auth, payload, nil, nil)
	if err != nil {
		return err
	}
	k, err := s.vapidKey()
	if err != nil {
		return err
	}
	jwt, err := vapidJWT(k, ep.Scheme+"://"+ep.Host, contact, time.Now().Add(12*time.Hour))
	if err != nil {
		return err
	}
	pub, err := vapidPublic(k)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", urgency)
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+pub)
	resp, err := pushHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("连不上推送服务 %s：%v", ep.Host, err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == 404 || resp.StatusCode == 410:
		return fmt.Errorf("%w（%s 返回 HTTP %d：%s）", errSubGone, ep.Host, resp.StatusCode, strings.TrimSpace(string(msg)))
	case resp.StatusCode >= 300:
		return fmt.Errorf("推送服务 %s 返回 HTTP %d：%s", ep.Host, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
