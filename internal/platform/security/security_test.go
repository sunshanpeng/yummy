package security

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestPasswordHashAndBounds(t *testing.T) {
	password := "correct-horse-example"
	a, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("密码盐必须随机")
	}
	if !VerifyPassword(a, password) || VerifyPassword(a, "wrong-password") {
		t.Fatal("密码校验错误")
	}
	for _, value := range []string{"", "$argon2id$v=19$m=999999999,t=2,p=1$bad$bad", "$argon2id$v=19$m=19456,t=0,p=1$bad$bad"} {
		if VerifyPassword(value, password) {
			t.Fatal("损坏散列不能通过")
		}
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("短密码不能通过")
	}
	if _, err := HashPassword(string(bytes.Repeat([]byte("x"), 129))); err == nil {
		t.Fatal("超长密码不能通过")
	}
}

func TestSecretBoxBindsIdentityAndUsesRandomNonce(t *testing.T) {
	box, err := NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	a, err := box.Encrypt([]byte("secret"), "app-1:variable:v1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := box.Encrypt([]byte("secret"), "app-1:variable:v1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("nonce必须随机")
	}
	plain, err := box.Decrypt(a, "app-1:variable:v1")
	if err != nil || string(plain) != "secret" {
		t.Fatalf("解密失败: %v", err)
	}
	if _, err := box.Decrypt(a, "app-2:variable:v1"); err == nil {
		t.Fatal("不能跨应用替换密文")
	}
	a[len(a)-1] ^= 1
	if _, err := box.Decrypt(a, "app-1:variable:v1"); err == nil {
		t.Fatal("篡改不能通过")
	}
	if _, err := NewSecretBox("weak-key"); err == nil {
		t.Fatal("弱主密钥不能通过")
	}
}

func TestRandomTokens(t *testing.T) {
	a, err := NewToken("ym_pat_")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken("ym_pat_")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || Digest(a) == a || len(Digest(a)) != 64 {
		t.Fatal("凭证随机性或摘要格式错误")
	}
}
